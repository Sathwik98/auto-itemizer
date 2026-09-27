package receipt

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"auto-itemizer/internal/db"
	"auto-itemizer/internal/expense"
	"auto-itemizer/internal/fileupload"
	"auto-itemizer/internal/ocr"
	"auto-itemizer/internal/receipt/parser"

	"github.com/shopspring/decimal"
)

var ctx = context.Background()

func fixturePath(parts ...string) string {
	return filepath.Join(append([]string{"..", "..", "fixtures"}, parts...)...)
}

// env holds real services over a temporary database, wired the way main.go
// will wire them, with the mock OCR over the fixtures.
type env struct {
	db       *db.DB
	files    *fileupload.Service
	expenses *expense.Service
	parser   *parser.Parser
	receipts *Service
	storage  string // folder the uploaded files go to
}

func newEnv(t *testing.T) *env {
	t.Helper()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	storage := t.TempDir()
	files, err := fileupload.New(storage)
	if err != nil {
		t.Fatal(err)
	}
	mock, err := ocr.NewMockProvider(
		[]string{fixturePath("task-a"), fixturePath("mock-ocr")},
		fixturePath("task-a", "receipt-clean.txt"))
	if err != nil {
		t.Fatal(err)
	}

	expenses := expense.New(d)
	names, err := expenses.TaxNames(ctx)
	if err != nil {
		t.Fatal(err)
	}
	e := &env{db: d, files: files, expenses: expenses, parser: parser.New(names), storage: storage}
	e.receipts = e.withOCR(mock, time.Second)
	expenses.SetParsedReceiptSource(e.receipts)
	return e
}

// withOCR returns another ReceiptService over the same database and services,
// with a different OCR provider.
func (e *env) withOCR(p ocr.Provider, timeout time.Duration) *Service {
	return New(e.db, e.files, ocr.New(p, timeout), e.expenses, e.parser)
}

func (e *env) upload(t *testing.T, fileName string) Receipt {
	t.Helper()
	r, err := e.receipts.Upload(ctx, fileName, "image/png", strings.NewReader("fake image bytes"))
	if err != nil {
		t.Fatalf("Upload %s: %v", fileName, err)
	}
	return r
}

func (e *env) process(t *testing.T, id string) expense.Expense {
	t.Helper()
	exp, err := e.receipts.Process(ctx, id)
	if err != nil {
		t.Fatalf("Process: %v", err)
	}
	return exp
}

func (e *env) count(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	if err := e.db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return n
}

func (e *env) details(t *testing.T, id string) Details {
	t.Helper()
	d, err := e.receipts.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get %s: %v", id, err)
	}
	return d
}

// checkInvariant fails the test unless the receipt is PROCESSED exactly when
// it has one active expense (ARCHITECTURE.md §3.2).
func (e *env) checkInvariant(t *testing.T, receiptID string) {
	t.Helper()
	d := e.details(t, receiptID)
	active := e.count(t, `SELECT count(*) FROM expense WHERE receipt_id = ? AND is_deleted = 0`, receiptID)
	if active > 1 || (d.Status == StatusProcessed) != (active == 1) {
		t.Errorf("receipt %s is %s with %d active expenses; want PROCESSED exactly when there is one", receiptID, d.Status, active)
	}
}

// fakeProvider is an OCR provider for tests. It returns err; or with hang set
// it waits until its context ends; or with cancel set it first cancels the
// caller's context, like a client that disconnects during OCR.
type fakeProvider struct {
	err    error
	hang   bool
	cancel context.CancelFunc
}

func (p fakeProvider) Extract(ctx context.Context, f fileupload.FileUpload) (string, error) {
	if p.cancel != nil {
		p.cancel()
	}
	if p.hang || p.cancel != nil {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return "", p.err
}

func TestUpload(t *testing.T) {
	e := newEnv(t)
	first := e.upload(t, "receipt-clean.png")
	second := e.upload(t, "receipt-clean.png")

	if first.ID == second.ID || first.Status != StatusUploaded {
		t.Errorf("uploads = %+v and %+v; want two UPLOADED receipts", first, second)
	}
	if n := e.count(t, `SELECT count(*) FROM receipts`); n != 2 {
		t.Errorf("%d receipts, want 2 (no duplicate check)", n)
	}
	files, err := os.ReadDir(filepath.Join(e.storage, "receipts"))
	if err != nil || len(files) != 2 {
		t.Errorf("stored files = %d (%v), want 2", len(files), err)
	}
	if d := e.details(t, first.ID); d.File.FileName != "receipt-clean.png" || d.ExpenseID != "" {
		t.Errorf("Get after upload = %+v", d)
	}
}

func TestUploadRemovesFileWhenTransactionFails(t *testing.T) {
	e := newEnv(t)
	if _, err := e.db.ExecContext(ctx, `
		CREATE TRIGGER refuse_receipts BEFORE INSERT ON receipts
		BEGIN SELECT RAISE(ABORT, 'insert refused by the test'); END`); err != nil {
		t.Fatal(err)
	}

	if _, err := e.receipts.Upload(ctx, "receipt-clean.png", "image/png", strings.NewReader("bytes")); err == nil {
		t.Fatal("Upload succeeded, want the trigger's error")
	}
	files, _ := os.ReadDir(filepath.Join(e.storage, "receipts"))
	if len(files) != 0 {
		t.Errorf("%d files left on disk, want 0", len(files))
	}
	if n := e.count(t, `SELECT count(*) FROM file_upload`); n != 0 {
		t.Errorf("%d file_upload rows, want 0", n)
	}
}

func TestProcessFixtures(t *testing.T) {
	e := newEnv(t)
	cases := []struct {
		fixture string
		status  string
		total   string
	}{
		{"receipt-clean", expense.StatusComplete, "17.85"},
		{"receipt-tax-only", expense.StatusNeedsReview, "24.00"},
		{"receipt-mismatch", expense.StatusNeedsReview, "18.50"},
	}
	for _, tc := range cases {
		r := e.upload(t, tc.fixture+".png")
		exp := e.process(t, r.ID)
		if exp.ItemizationStatus != tc.status || exp.Total.StringFixed(2) != tc.total {
			t.Errorf("%s: status %s total %s, want %s %s", tc.fixture, exp.ItemizationStatus, exp.Total.StringFixed(2), tc.status, tc.total)
		}

		d := e.details(t, r.ID)
		if d.Status != StatusProcessed || d.FailureReason != "" || d.ExpenseID != exp.ID {
			t.Errorf("%s: Get = %+v, want PROCESSED with expense %s", tc.fixture, d.Receipt, exp.ID)
		}
		want, _ := os.ReadFile(fixturePath("task-a", tc.fixture+".txt"))
		stored, err := selectActiveOCR(ctx, e.db, r.ID)
		if err != nil || stored != string(want) {
			t.Errorf("%s: stored OCR text differs from the fixture (%v)", tc.fixture, err)
		}
		e.checkInvariant(t, r.ID)
	}
}

func TestProcessGuardFixtures(t *testing.T) {
	e := newEnv(t)
	for fixture, code := range map[string]string{
		"unreadable":        CodeOCRUnreadable,
		"not-a-receipt":     CodeNotAReceipt,
		"header-incomplete": CodeHeaderIncomplete,
		"invalid-values":    CodeInvalidReceiptValues,
	} {
		r := e.upload(t, fixture+".png")
		_, err := e.receipts.Process(ctx, r.ID)
		if got := codeOf(t, err); got != code {
			t.Errorf("%s: code %q, want %q", fixture, got, code)
		}
		d := e.details(t, r.ID)
		if d.Status != StatusFailed || d.FailureReason != code || d.ExpenseID != "" {
			t.Errorf("%s: Get = %+v, want FAILED with %s and no expense", fixture, d.Receipt, code)
		}
		// The text that failed is kept for debugging.
		if n := e.count(t, `SELECT count(*) FROM receipt_ocr WHERE receipt_id = ? AND is_deleted = 0`, r.ID); n != 1 {
			t.Errorf("%s: %d active OCR rows, want 1", fixture, n)
		}
		e.checkInvariant(t, r.ID)
	}
}

func TestProcessOCRFailed(t *testing.T) {
	e := newEnv(t)
	r := e.upload(t, "receipt-clean.png")
	first := e.process(t, r.ID)

	failing := e.withOCR(fakeProvider{err: errors.New("vendor down")}, time.Second)
	_, err := failing.Process(ctx, r.ID)
	if got := codeOf(t, err); got != CodeOCRFailed {
		t.Fatalf("code %q, want OCR_FAILED", got)
	}
	d := e.details(t, r.ID)
	if d.Status != StatusFailed || d.FailureReason != CodeOCRFailed || d.ExpenseID != "" {
		t.Errorf("Get = %+v, want FAILED/OCR_FAILED with no expense", d.Receipt)
	}
	if _, err := e.expenses.Get(ctx, first.ID); !errors.Is(err, expense.ErrNotFound) {
		t.Errorf("the old expense: %v, want it soft-deleted", err)
	}
	// No OCR row is written, so the previous one stays active.
	if n := e.count(t, `SELECT count(*) FROM receipt_ocr WHERE receipt_id = ?`, r.ID); n != 1 {
		t.Errorf("%d OCR rows, want the 1 from the first run", n)
	}
	e.checkInvariant(t, r.ID)

	// Processing again with a working OCR recovers the receipt.
	e.process(t, r.ID)
	if d := e.details(t, r.ID); d.Status != StatusProcessed || d.FailureReason != "" {
		t.Errorf("after the retry: %+v, want PROCESSED", d.Receipt)
	}
	e.checkInvariant(t, r.ID)
}

// TestProcessOCRTimeout: an OCR timeout is guard 1 (OCR_FAILED), unlike a
// client that disconnects, which writes nothing (next test).
func TestProcessOCRTimeout(t *testing.T) {
	e := newEnv(t)
	r := e.upload(t, "receipt-clean.png")
	slow := e.withOCR(fakeProvider{hang: true}, 20*time.Millisecond)
	if _, err := slow.Process(ctx, r.ID); codeOf(t, err) != CodeOCRFailed {
		t.Errorf("OCR timeout: %v, want OCR_FAILED", err)
	}
}

func TestProcessClientGone(t *testing.T) {
	e := newEnv(t)
	r := e.upload(t, "receipt-clean.png")

	requestCtx, cancel := context.WithCancel(ctx)
	svc := e.withOCR(fakeProvider{cancel: cancel}, time.Second)
	if _, err := svc.Process(requestCtx, r.ID); !errors.Is(err, context.Canceled) {
		t.Fatalf("Process = %v, want context.Canceled", err)
	}
	d := e.details(t, r.ID)
	if d.Status != StatusUploaded || d.UpdatedAt != r.UpdatedAt {
		t.Errorf("after the client left: %+v, want the receipt unchanged", d.Receipt)
	}
}

func TestProcessLiveOCRNotConfigured(t *testing.T) {
	e := newEnv(t)
	r := e.upload(t, "receipt-clean.png")
	live := e.withOCR(ocr.LiveProvider{}, time.Second)

	if _, err := live.Process(ctx, r.ID); !errors.Is(err, ErrLiveOCRNotConfigured) {
		t.Fatalf("Process = %v, want ErrLiveOCRNotConfigured", err)
	}
	d := e.details(t, r.ID)
	if d.Status != StatusUploaded || d.UpdatedAt != r.UpdatedAt {
		t.Errorf("after 501: %+v, want the receipt unchanged", d.Receipt)
	}
	if n := e.count(t, `SELECT count(*) FROM receipt_ocr`); n != 0 {
		t.Errorf("%d OCR rows, want 0", n)
	}
}

func TestUnknownReceipt(t *testing.T) {
	e := newEnv(t)
	if _, err := e.receipts.Process(ctx, "no-such-id"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Process: %v, want ErrNotFound", err)
	}
	if _, err := e.receipts.Get(ctx, "no-such-id"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get: %v, want ErrNotFound", err)
	}
}

func TestReprocess(t *testing.T) {
	e := newEnv(t)
	r := e.upload(t, "receipt-clean.png")
	first := e.process(t, r.ID)
	second := e.process(t, r.ID)

	if second.ID == first.ID {
		t.Error("reprocessing kept the expense id, want a new expense")
	}
	if _, err := e.expenses.Get(ctx, first.ID); !errors.Is(err, expense.ErrNotFound) {
		t.Errorf("first expense: %v, want it soft-deleted", err)
	}
	if n := e.count(t, `SELECT count(*) FROM receipt_ocr WHERE receipt_id = ? AND is_deleted = 0`, r.ID); n != 1 {
		t.Errorf("%d active OCR rows, want 1", n)
	}
	if n := e.count(t, `SELECT count(*) FROM receipt_ocr WHERE receipt_id = ?`, r.ID); n != 2 {
		t.Errorf("%d OCR rows in total, want 2 (history kept)", n)
	}
	if d := e.details(t, r.ID); d.ExpenseID != second.ID {
		t.Errorf("Get expense id = %s, want the new one %s", d.ExpenseID, second.ID)
	}
	e.checkInvariant(t, r.ID)
}

func TestMockFallback(t *testing.T) {
	e := newEnv(t)
	r := e.upload(t, "my-own-receipt.jpg") // no fixture has this name
	exp := e.process(t, r.ID)
	if exp.Merchant != "Cafe Mitte" || exp.ItemizationStatus != expense.StatusComplete {
		t.Errorf("fallback: %s %s, want the gold receipt (Cafe Mitte, COMPLETE)", exp.Merchant, exp.ItemizationStatus)
	}
}

// TestReitemizeEndToEnd re-itemizes through the real GetParsedReceipt: after
// a PATCH, re-itemize goes back to the automatic result.
func TestReitemizeEndToEnd(t *testing.T) {
	e := newEnv(t)
	r := e.upload(t, "receipt-mismatch.png")
	exp := e.process(t, r.ID)

	items := []expense.ItemInput{
		{ID: exp.LineItems[0].ID, Description: exp.LineItems[0].Description, Amount: exp.LineItems[0].Amount},
		{ID: exp.LineItems[1].ID, Description: exp.LineItems[1].Description, Amount: exp.LineItems[1].Amount},
		{Description: "Minibar", Amount: decimal.RequireFromString("6.60")},
	}
	if patched, err := e.expenses.PatchItems(ctx, exp.ID, items); err != nil || patched.ItemizationStatus != expense.StatusComplete {
		t.Fatalf("PatchItems: %v, %v; want COMPLETE", err, patched.ItemizationStatus)
	}

	again, err := e.expenses.Reitemize(ctx, exp.ID)
	if err != nil {
		t.Fatalf("Reitemize: %v", err)
	}
	if again.ItemizationStatus != expense.StatusNeedsReview || len(again.LineItems) != 2 {
		t.Errorf("after re-itemize: %s with %d items, want NEEDS_REVIEW with Water and Snacks", again.ItemizationStatus, len(again.LineItems))
	}
	e.checkInvariant(t, r.ID)
}

func TestGetParsedReceiptWithoutOCR(t *testing.T) {
	e := newEnv(t)
	r := e.upload(t, "receipt-clean.png")
	if _, err := e.receipts.GetParsedReceipt(ctx, r.ID); !errors.Is(err, errNoOCR) {
		t.Errorf("GetParsedReceipt before process: %v, want errNoOCR", err)
	}
}

// TestSaveWithStaleReceiptConflicts covers the 409 CONFLICT: another request
// moved the receipt after this one read it.
func TestSaveWithStaleReceiptConflicts(t *testing.T) {
	e := newEnv(t)
	r := e.upload(t, "receipt-clean.png")
	stale, err := selectReceipt(ctx, e.db, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	e.process(t, r.ID) // another request processes it first
	before := e.details(t, r.ID)

	if _, err := e.receipts.saveOCR(ctx, stale, "TOTAL 1.00"); !errors.Is(err, db.ErrConflict) {
		t.Errorf("saveOCR with a stale receipt: %v, want db.ErrConflict", err)
	}
	after := e.details(t, r.ID)
	if after.Receipt != before.Receipt || after.ExpenseID != before.ExpenseID {
		t.Errorf("a conflicting save wrote: %+v, want %+v", after, before)
	}
	if n := e.count(t, `SELECT count(*) FROM receipt_ocr WHERE receipt_id = ?`, r.ID); n != 1 {
		t.Errorf("%d OCR rows, want 1", n)
	}
	e.checkInvariant(t, r.ID)
}
