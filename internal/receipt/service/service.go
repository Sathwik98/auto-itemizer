// Package service is the ReceiptService (ARCHITECTURE.md §1). It stores
// uploaded receipts, runs OCR, the guards and the parser on them, and is the
// only code that writes the receipts and receipt_ocr tables.
package service

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"log/slog"
	"time"

	"auto-itemizer/internal/db"
	expensecore "auto-itemizer/internal/expense/core"
	expenseservice "auto-itemizer/internal/expense/service"
	fileuploadcore "auto-itemizer/internal/fileupload/core"
	fileuploadservice "auto-itemizer/internal/fileupload/service"
	"auto-itemizer/internal/metrics"
	"auto-itemizer/internal/ocr"
	"auto-itemizer/internal/receipt/core"
	"auto-itemizer/internal/receipt/core/parser"
	"auto-itemizer/internal/receipt/repository"

	"github.com/google/uuid"
)

// Service is the ReceiptService.
type Service struct {
	db       *db.DB
	files    *fileuploadservice.Service
	ocr      *ocr.Service
	expenses *expenseservice.Service
	parser   *parser.Parser
}

// The compiler checks that Service is what ExpenseService's re-itemize needs.
var _ expenseservice.ParsedReceiptSource = (*Service)(nil)

// New returns a Service. The parser is built with the tax names from
// tax_master (see expenseservice.Service.TaxNames).
func New(d *db.DB, files *fileuploadservice.Service, o *ocr.Service, expenses *expenseservice.Service, p *parser.Parser) *Service {
	return &Service{db: d, files: files, ocr: o, expenses: expenses, parser: p}
}

// Details is what GET /receipts/{id} returns (ARCHITECTURE.md §3.3): the
// receipt, its file and the id of its active expense.
type Details struct {
	core.Receipt
	File      fileuploadcore.FileUpload
	ExpenseID string // "" when the receipt has no active expense
}

// Upload stores an uploaded file and creates its receipt (ARCHITECTURE.md
// §3.1). The file and both rows are saved together: if the transaction fails,
// the file is removed again.
func (s *Service) Upload(ctx context.Context, fileName, contentType string, body io.Reader) (core.Receipt, error) {
	now := db.Now()
	r := core.Receipt{ID: uuid.NewString(), Status: core.StatusUploaded, CreatedAt: now, UpdatedAt: now}
	var saved *fileuploadcore.FileUpload // set once the file is on disk

	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		f, err := s.files.Save(ctx, tx, fileName, contentType, body)
		if err != nil {
			return err // Save removes its own file when it fails
		}
		saved = &f
		r.FileID = f.ID
		return repository.InsertReceipt(ctx, tx, r)
	})
	if err != nil {
		if saved != nil {
			// The rows were rolled back, so the file must go too.
			if delErr := s.files.Delete(*saved); delErr != nil {
				slog.Error("upload failed and its file could not be removed",
					"file_path", saved.FilePath, "error", delErr)
			}
		}
		return core.Receipt{}, err
	}
	return r, nil
}

// Process runs OCR on the receipt, checks the text with the guards, parses it
// and creates the expense (ARCHITECTURE.md §3.2). Besides the expense, it can
// return:
//   - core.ErrNotFound for an unknown receipt;
//   - core.ErrLiveOCRNotConfigured when MOCK_OCR is false (nothing is written);
//   - a *core.GuardError when OCR fails or a guard rejects the text (the receipt
//     is then FAILED with that code);
//   - db.ErrConflict when another request changed the receipt meanwhile.
func (s *Service) Process(ctx context.Context, id string) (expensecore.Expense, error) {
	r, err := repository.SelectReceipt(ctx, s.db, id)
	if err != nil {
		return expensecore.Expense{}, err
	}
	f, err := s.files.Get(ctx, s.db, r.FileID)
	if err != nil {
		return expensecore.Expense{}, err
	}

	// The OCR call can be slow, so nothing is written until it returns.
	text, err := s.ocr.Extract(ctx, f)
	switch {
	case errors.Is(err, ocr.ErrNotConfigured):
		return expensecore.Expense{}, core.ErrLiveOCRNotConfigured
	case err != nil && ctx.Err() != nil:
		// The client went away: not an OCR failure, so nothing is saved.
		return expensecore.Expense{}, ctx.Err()
	case err != nil:
		return expensecore.Expense{}, s.saveOCRFailed(ctx, r, err)
	}

	r, err = s.saveOCR(ctx, r, text)
	if err != nil {
		return expensecore.Expense{}, err
	}

	parsed, err := core.CheckAndParse(s.parser, text, time.Now())
	if err != nil {
		var guardErr *core.GuardError
		if !errors.As(err, &guardErr) {
			return expensecore.Expense{}, err
		}
		return expensecore.Expense{}, s.saveGuardFailure(ctx, r, guardErr)
	}
	return s.saveExpense(ctx, r, parsed)
}

// saveOCRFailed records guard 1: the OCR call failed or timed out. The active
// expense is removed in the same save, so a FAILED receipt never keeps one.
// No OCR row is written, so the previous one stays active.
func (s *Service) saveOCRFailed(ctx context.Context, r core.Receipt, cause error) error {
	slog.Warn("OCR failed", "receipt_id", r.ID, "error", cause)
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := repository.UpdateStatus(ctx, tx, r, core.StatusFailed, core.CodeOCRFailed); err != nil {
			return err
		}
		return s.expenses.SoftDeleteActiveForReceipt(ctx, tx, r.ID)
	})
	if err != nil {
		return err
	}
	metrics.RecordReceiptOutcome(core.CodeOCRFailed)
	return &core.GuardError{Code: core.CodeOCRFailed, Message: "the OCR call failed or timed out; process the receipt again to retry"}
}

// saveOCR stores the OCR text (save 3 of ARCHITECTURE.md §3.2). In one
// transaction it moves the receipt to OCR_EXTRACTED, removes the old expense
// and replaces the active OCR row. It returns the receipt as written.
func (s *Service) saveOCR(ctx context.Context, r core.Receipt, text string) (core.Receipt, error) {
	var written core.Receipt
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		var err error
		if written, err = repository.UpdateStatus(ctx, tx, r, core.StatusOCRExtracted, ""); err != nil {
			return err
		}
		if err := s.expenses.SoftDeleteActiveForReceipt(ctx, tx, r.ID); err != nil {
			return err
		}
		now := db.Now()
		if err := repository.SoftDeleteActiveOCR(ctx, tx, r.ID, now); err != nil {
			return err
		}
		return repository.InsertOCR(ctx, tx, r.ID, text, now)
	})
	if err != nil {
		return core.Receipt{}, err
	}
	return written, nil
}

// saveGuardFailure marks the receipt FAILED with the guard's code and returns
// the guard's error. The OCR row stays, so the text that failed can be read.
func (s *Service) saveGuardFailure(ctx context.Context, r core.Receipt, guardErr *core.GuardError) error {
	if _, err := repository.UpdateStatus(ctx, s.db, r, core.StatusFailed, guardErr.Code); err != nil {
		return err
	}
	slog.Warn("receipt failed a guard", "receipt_id", r.ID, "code", guardErr.Code, "reason", guardErr.Message)
	metrics.RecordReceiptOutcome(guardErr.Code)
	return guardErr
}

// saveExpense creates the expense and marks the receipt PROCESSED, in one
// transaction (save 5 of ARCHITECTURE.md §3.2).
func (s *Service) saveExpense(ctx context.Context, r core.Receipt, parsed parser.ParsedReceipt) (expensecore.Expense, error) {
	var e expensecore.Expense
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if _, err := repository.UpdateStatus(ctx, tx, r, core.StatusProcessed, ""); err != nil {
			return err
		}
		created, err := s.expenses.CreateFromReceipt(ctx, tx, r.ID, parsed)
		e = created
		return err
	})
	if err != nil {
		return expensecore.Expense{}, err
	}
	slog.Info("receipt processed", "receipt_id", r.ID, "transaction_id", e.ID, "itemize_status", e.ItemizationStatus)
	metrics.RecordReceiptOutcome(e.ItemizationStatus)
	return e, nil
}

// Get returns the receipt with its file and the id of its active expense
// (ARCHITECTURE.md §3.3), read from one snapshot so they always agree.
func (s *Service) Get(ctx context.Context, id string) (Details, error) {
	var d Details
	err := s.db.InReadTx(ctx, func(tx *sql.Tx) error {
		r, err := repository.SelectReceipt(ctx, tx, id)
		if err != nil {
			return err
		}
		f, err := s.files.Get(ctx, tx, r.FileID)
		if err != nil {
			return err
		}
		expenseID, err := s.expenses.ActiveIDForReceipt(ctx, tx, id)
		if err != nil {
			return err
		}
		d = Details{Receipt: r, File: f, ExpenseID: expenseID}
		return nil
	})
	if err != nil {
		return Details{}, err
	}
	return d, nil
}

// GetParsedReceipt parses the receipt's active OCR text, for re-itemize
// (ARCHITECTURE.md §3.5). The guards aren't run again: the text passed them
// when the expense was created.
func (s *Service) GetParsedReceipt(ctx context.Context, receiptID string) (parser.ParsedReceipt, error) {
	text, err := repository.SelectActiveOCR(ctx, s.db, receiptID)
	if err != nil {
		return parser.ParsedReceipt{}, err
	}
	return s.parser.Parse(text), nil
}
