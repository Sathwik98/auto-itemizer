package expense

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"auto-itemizer/internal/db"
	"auto-itemizer/internal/receipt/parser"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// Service is the ExpenseService. It is the only code that writes the expense,
// expense_tax, expense_line_item and tax_master tables.
type Service struct {
	db       *db.DB
	receipts ParsedReceiptSource // for Reitemize; see SetParsedReceiptSource
}

// New returns a Service. Call SetParsedReceiptSource before Reitemize.
func New(d *db.DB) *Service {
	return &Service{db: d}
}

// SetParsedReceiptSource connects ReceiptService, where re-itemize reads the
// stored OCR text. The two services need each other, so main.go builds both
// and then connects this direction.
func (s *Service) SetParsedReceiptSource(src ParsedReceiptSource) {
	s.receipts = src
}

// CreateFromReceipt creates the expense for a processed receipt: the header,
// one tax row per printed tax, and the line items with their status
// (ARCHITECTURE.md §3.2, save 5). It runs in the caller's transaction q.
func (s *Service) CreateFromReceipt(ctx context.Context, q db.Querier, receiptID string, p parser.ParsedReceipt) (Expense, error) {
	// The guards have checked these; refuse rather than crash if they didn't run.
	if p.Total == nil {
		return Expense{}, errors.New("create expense: the receipt has no total")
	}
	for _, tax := range p.Taxes {
		if tax.Rate == nil {
			return Expense{}, fmt.Errorf("create expense: the %s line has no rate", tax.Name)
		}
	}

	now := db.Now()
	taxes := make([]decimal.Decimal, len(p.Taxes))
	for i, tax := range p.Taxes {
		taxes[i] = tax.Amount
	}
	lines, status := itemize(p.CandidateLines, taxes, *p.Total)

	e := Expense{
		ID:                uuid.NewString(),
		ReceiptID:         receiptID,
		Merchant:          p.Merchant,
		Date:              p.Date,
		Currency:          p.Currency,
		Total:             *p.Total,
		ItemizationStatus: status,
		UpdatedAt:         now,
	}
	if err := insertExpense(ctx, q, e, now); err != nil {
		return Expense{}, err
	}

	for _, printed := range p.Taxes {
		taxMasterID, err := upsertTaxName(ctx, q, printed.Name, now)
		if err != nil {
			return Expense{}, err
		}
		tax := Tax{
			Name:          printed.Name,
			Rate:          *printed.Rate,
			TaxableAmount: taxableAmount(p, printed),
			Amount:        printed.Amount,
		}
		if err := insertExpenseTax(ctx, q, e.ID, taxMasterID, tax, now); err != nil {
			return Expense{}, err
		}
		e.Taxes = append(e.Taxes, tax)
	}

	for _, line := range lines {
		item, err := insertLineItem(ctx, q, e.ID, line.Description, line.Amount, db.ActorSystem, now)
		if err != nil {
			return Expense{}, err
		}
		e.LineItems = append(e.LineItems, item)
	}
	return e, nil
}

// SoftDeleteActiveForReceipt marks the receipt's active expense deleted, if it
// has one. It runs in the caller's transaction q (ARCHITECTURE.md §3.2).
func (s *Service) SoftDeleteActiveForReceipt(ctx context.Context, q db.Querier, receiptID string) error {
	return softDeleteActiveExpense(ctx, q, receiptID, db.Now())
}

// ActiveIDForReceipt returns the id of the receipt's active expense, or ""
// when it has none (ARCHITECTURE.md §3.3).
func (s *Service) ActiveIDForReceipt(ctx context.Context, q db.Querier, receiptID string) (string, error) {
	return selectActiveExpenseID(ctx, q, receiptID)
}

// Get returns the active expense with its taxes and active items
// (ARCHITECTURE.md §3.4), all read from one snapshot. It returns ErrNotFound
// for an unknown or deleted expense.
func (s *Service) Get(ctx context.Context, id string) (Expense, error) {
	var e Expense
	err := s.db.InReadTx(ctx, func(tx *sql.Tx) error {
		loaded, err := load(ctx, tx, id)
		e = loaded
		return err
	})
	if err != nil {
		return Expense{}, err
	}
	return e, nil
}

// Reitemize runs itemize again on the stored OCR text and replaces the line
// items (ARCHITECTURE.md §3.5). The header and taxes are not touched, and no
// second expense is created.
func (s *Service) Reitemize(ctx context.Context, id string) (Expense, error) {
	if s.receipts == nil {
		return Expense{}, errors.New("re-itemize: no parsed receipt source; call SetParsedReceiptSource")
	}
	e, err := s.Get(ctx, id)
	if err != nil {
		return Expense{}, err
	}
	parsed, err := s.receipts.GetParsedReceipt(ctx, e.ReceiptID)
	if err != nil {
		return Expense{}, fmt.Errorf("re-itemize expense %s: %w", id, err)
	}
	lines, status := itemize(parsed.CandidateLines, taxAmounts(e.Taxes), e.Total)
	return s.saveReitemize(ctx, e, lines, status)
}

// saveReitemize writes a re-itemize result for e, as read by Reitemize.
func (s *Service) saveReitemize(ctx context.Context, e Expense, lines []parser.Line, status string) (Expense, error) {
	var out Expense
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := updateItemizationStatus(ctx, tx, e.ID, status, e.UpdatedAt, db.ActorSystem); err != nil {
			return err
		}
		now := db.Now()
		if err := softDeleteActiveLineItems(ctx, tx, e.ID, db.ActorSystem, now); err != nil {
			return err
		}
		for _, line := range lines {
			if _, err := insertLineItem(ctx, tx, e.ID, line.Description, line.Amount, db.ActorSystem, now); err != nil {
				return err
			}
		}
		loaded, err := load(ctx, tx, e.ID)
		out = loaded
		return err
	})
	if err != nil {
		return Expense{}, err
	}
	return out, nil
}

// PatchItems replaces the expense's items with the complete list the user
// sent (ARCHITECTURE.md §3.6). If the items plus the stored taxes don't add up
// to the stored total, it returns a *MismatchError and writes nothing. Items
// that didn't change keep their ids.
func (s *Service) PatchItems(ctx context.Context, id string, items []ItemInput) (Expense, error) {
	e, err := s.Get(ctx, id)
	if err != nil {
		return Expense{}, err
	}
	if err := checkItems(e, items); err != nil {
		return Expense{}, err
	}
	amounts := make([]decimal.Decimal, len(items))
	for i, item := range items {
		amounts[i] = item.Amount
	}
	if mismatch := reconcile(amounts, taxAmounts(e.Taxes), e.Total); mismatch != nil {
		return Expense{}, mismatch
	}
	return s.savePatch(ctx, e, items)
}

// checkItems applies the PATCH rules: every id is an active item of e and
// appears once, every description is non-empty, and every amount is non-zero
// with at most two decimals.
func checkItems(e Expense, items []ItemInput) error {
	active := make(map[string]bool, len(e.LineItems))
	for _, item := range e.LineItems {
		active[item.ID] = true
	}
	seen := make(map[string]bool, len(items))
	for i, item := range items {
		n := i + 1 // position in the request, for the message
		if item.ID != "" {
			if !active[item.ID] {
				return fmt.Errorf("%w: item %d has id %s", ErrUnknownItem, n, item.ID)
			}
			if seen[item.ID] {
				return fmt.Errorf("%w: item %d repeats id %s", ErrInvalidItem, n, item.ID)
			}
			seen[item.ID] = true
		}
		switch {
		case item.Description == "":
			return fmt.Errorf("%w: item %d has no description", ErrInvalidItem, n)
		case item.Amount.IsZero():
			return fmt.Errorf("%w: item %d has amount 0", ErrInvalidItem, n)
		case !fitsMoney(item.Amount):
			return fmt.Errorf("%w: item %d amount %s has more than two decimals", ErrInvalidItem, n, item.Amount)
		}
	}
	return nil
}

// savePatch writes a checked PATCH for e, as read by PatchItems. An item sent
// with its id, description and amount unchanged keeps its row. Every other
// active item is soft-deleted, and the changed and new items are inserted in
// request order, after the unchanged ones.
func (s *Service) savePatch(ctx context.Context, e Expense, items []ItemInput) (Expense, error) {
	old := make(map[string]LineItem, len(e.LineItems))
	for _, item := range e.LineItems {
		old[item.ID] = item
	}
	unchanged := make(map[string]bool)
	for _, item := range items {
		if prev, ok := old[item.ID]; ok && prev.Description == item.Description && prev.Amount.Equal(item.Amount) {
			unchanged[item.ID] = true
		}
	}

	var out Expense
	err := s.db.InTx(ctx, func(tx *sql.Tx) error {
		if err := updateItemizationStatus(ctx, tx, e.ID, StatusComplete, e.UpdatedAt, db.ActorUser); err != nil {
			return err
		}
		now := db.Now()
		for _, item := range e.LineItems {
			if !unchanged[item.ID] {
				if err := softDeleteLineItem(ctx, tx, item.ID, db.ActorUser, now); err != nil {
					return err
				}
			}
		}
		for _, item := range items {
			if !unchanged[item.ID] {
				if _, err := insertLineItem(ctx, tx, e.ID, item.Description, item.Amount, db.ActorUser, now); err != nil {
					return err
				}
			}
		}
		loaded, err := load(ctx, tx, e.ID)
		out = loaded
		return err
	})
	if err != nil {
		return Expense{}, err
	}
	return out, nil
}

// TaxNames returns every name in tax_master. main.go builds the parser with
// them at startup.
func (s *Service) TaxNames(ctx context.Context) ([]string, error) {
	return selectTaxNames(ctx, s.db)
}

// load reads an active expense with its taxes and active items.
func load(ctx context.Context, q db.Querier, id string) (Expense, error) {
	e, err := selectActiveExpense(ctx, q, id)
	if err != nil {
		return Expense{}, err
	}
	if e.Taxes, err = selectExpenseTaxes(ctx, q, id); err != nil {
		return Expense{}, err
	}
	if e.LineItems, err = selectActiveLineItems(ctx, q, id); err != nil {
		return Expense{}, err
	}
	return e, nil
}
