package service

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"auto-itemizer/internal/db"
	"auto-itemizer/internal/expense/core"
	"auto-itemizer/internal/receipt/core/parser"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

var ctx = context.Background()

// fixtures is the brief's fixture folder, seen from this package's folder.
var fixtures = filepath.Join("..", "..", "..", "fixtures", "task-a")

var testParser = parser.New([]string{"VAT", "GST", "QST", "TAX", "CGST", "SGST"})

func dec(s string) decimal.Decimal {
	return decimal.RequireFromString(s)
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	text, err := os.ReadFile(filepath.Join(fixtures, name+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(text)
}

// header is a receipt header the guards would accept, for texts built here.
const header = "MERCHANT: Test Shop\nDATE: 2026-03-12\nCURRENCY: EUR\n"

// setup returns a Service over a temporary database.
func setup(t *testing.T) (*Service, *db.DB) {
	t.Helper()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return New(d), d
}

// newReceipt inserts a file_upload row and a receipts row with plain SQL: the
// receipt packages import this one, so ReceiptService can't be used here, and
// the foreign keys need both rows.
func newReceipt(t *testing.T, d *db.DB) string {
	t.Helper()
	fileID, receiptID, now := uuid.NewString(), uuid.NewString(), db.Now()
	mustExec(t, d, `
		INSERT INTO file_upload (id, file_path, file_name, content_type, size_bytes,
		                         created_at, created_by, updated_at, updated_by)
		VALUES (?, 'storage/receipts/x.txt', 'x.txt', 'text/plain', 1, ?, 'system', ?, 'system')`,
		fileID, now, now)
	mustExec(t, d, `
		INSERT INTO receipts (id, file_id, status, created_at, created_by, updated_at, updated_by)
		VALUES (?, ?, 'OCR_EXTRACTED', ?, 'system', ?, 'system')`,
		receiptID, fileID, now, now)
	return receiptID
}

// create parses text and creates the expense of a new receipt in one
// transaction, the way ReceiptService will.
func create(t *testing.T, s *Service, d *db.DB, text string) core.Expense {
	t.Helper()
	receiptID := newReceipt(t, d)
	var e core.Expense
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		created, err := s.CreateFromReceipt(ctx, tx, receiptID, testParser.Parse(text))
		e = created
		return err
	})
	if err != nil {
		t.Fatalf("CreateFromReceipt: %v", err)
	}
	return e
}

func mustExec(t *testing.T, d *db.DB, query string, args ...any) {
	t.Helper()
	if _, err := d.ExecContext(ctx, query, args...); err != nil {
		t.Fatalf("exec: %v", err)
	}
}

// queryString runs a query that returns one text value.
func queryString(t *testing.T, d *db.DB, query string, args ...any) string {
	t.Helper()
	var v string
	if err := d.QueryRowContext(ctx, query, args...).Scan(&v); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return v
}

func count(t *testing.T, d *db.DB, query string, args ...any) int {
	t.Helper()
	var n int
	if err := d.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	return n
}

// summary writes out what the tests compare in an expense, with fixed
// decimal formats, so two expenses can be compared as strings.
func summary(e core.Expense) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s %s total=%s status=%s", e.Merchant, e.Date, e.Currency, e.Total.StringFixed(2), e.ItemizationStatus)
	for _, tax := range e.Taxes {
		taxable := "none"
		if tax.TaxableAmount != nil {
			taxable = tax.TaxableAmount.StringFixed(2)
		}
		fmt.Fprintf(&b, "\n  tax %s rate=%s taxable=%s amount=%s", tax.Name, tax.Rate, taxable, tax.Amount.StringFixed(2))
	}
	for _, item := range e.LineItems {
		fmt.Fprintf(&b, "\n  item %s %q %s", item.ID, item.Description, item.Amount.StringFixed(2))
	}
	return b.String()
}

// mustGet reads an expense that must exist.
func mustGet(t *testing.T, s *Service, id string) core.Expense {
	t.Helper()
	e, err := s.Get(ctx, id)
	if err != nil {
		t.Fatalf("Get %s: %v", id, err)
	}
	return e
}

func TestCreateFromReceiptFixtures(t *testing.T) {
	s, _ := setup(t)
	d := s.db
	cases := []struct {
		fixture string
		status  string
		items   []string
		taxable string
	}{
		{"receipt-clean", core.StatusComplete, []string{"Espresso", "Sandwich", "Mineral water"}, "15.00"},
		{"receipt-tax-only", core.StatusNeedsReview, nil, "20.17"},
		{"receipt-mismatch", core.StatusNeedsReview, []string{"Water", "Snacks"}, "10.00"},
	}
	for _, tc := range cases {
		e := create(t, s, d, readFixture(t, tc.fixture))

		var items []string
		for _, item := range e.LineItems {
			items = append(items, item.Description)
		}
		if e.ItemizationStatus != tc.status || !slices.Equal(items, tc.items) {
			t.Errorf("%s: status %s, items %v; want %s, %v", tc.fixture, e.ItemizationStatus, items, tc.status, tc.items)
		}
		if len(e.Taxes) != 1 || e.Taxes[0].Name != "VAT" || !e.Taxes[0].Rate.Equal(dec("0.19")) ||
			e.Taxes[0].TaxableAmount == nil || !e.Taxes[0].TaxableAmount.Equal(dec(tc.taxable)) {
			t.Errorf("%s: taxes %+v, want VAT 0.19 with taxable %s", tc.fixture, e.Taxes, tc.taxable)
		}

		// What Get reads back is what CreateFromReceipt returned.
		if got := mustGet(t, s, e.ID); summary(got) != summary(e) {
			t.Errorf("%s: Get returned\n%s\nwant\n%s", tc.fixture, summary(got), summary(e))
		}
	}
}

func TestCreateStoresRatesExactly(t *testing.T) {
	s, d := setup(t)

	// Two taxes share the subtotal, so neither gets a taxable amount.
	e := create(t, s, d, header+"Poutine 10.00\nSubtotal 10.00\nGST 5% 0.50\nQST 9.975% 1.00\nTOTAL 11.50")
	got := mustGet(t, s, e.ID)
	if len(got.Taxes) != 2 || got.Taxes[1].Name != "QST" || got.Taxes[1].Rate.String() != "0.09975" ||
		got.Taxes[0].TaxableAmount != nil || got.Taxes[1].TaxableAmount != nil {
		t.Errorf("taxes = %s; want GST 0.05 and QST 0.09975, both without taxable", summary(got))
	}
	stored := queryString(t, d, `SELECT rate FROM expense_tax WHERE expense_id = ? ORDER BY rowid LIMIT 1 OFFSET 1`, e.ID)
	if stored != "0.09975" {
		t.Errorf("stored QST rate = %q, want \"0.09975\" (not rounded)", stored)
	}
	if got.ItemizationStatus != core.StatusComplete {
		t.Errorf("status = %s, want COMPLETE (10.00 + 0.50 + 1.00 = 11.50)", got.ItemizationStatus)
	}

	// A base printed on the tax line becomes its taxable amount.
	e = create(t, s, d, header+"Food 10.00\nVAT 19%  10.00  1.90\nTOTAL 11.90")
	got = mustGet(t, s, e.ID)
	if len(got.Taxes) != 1 || got.Taxes[0].TaxableAmount == nil || !got.Taxes[0].TaxableAmount.Equal(dec("10.00")) {
		t.Errorf("printed base: %s; want taxable 10.00", summary(got))
	}
}

func TestCreateWithoutTaxes(t *testing.T) {
	s, d := setup(t)
	e := create(t, s, d, header+"Book 12.00\nTOTAL 12.00")
	if e.ItemizationStatus != core.StatusComplete || len(e.Taxes) != 0 {
		t.Errorf("no taxes: %s; want COMPLETE with no tax rows", summary(e))
	}
	if n := count(t, d, `SELECT count(*) FROM expense_tax WHERE expense_id = ?`, e.ID); n != 0 {
		t.Errorf("%d expense_tax rows, want 0", n)
	}
}

func TestCreateAddsNewTaxNameOnce(t *testing.T) {
	s, d := setup(t)
	text := header + "Burger 10.00\nState Tax 6% 0.60\nTOTAL 10.60"
	first := create(t, s, d, text)
	second := create(t, s, d, text)

	if n := count(t, d, `SELECT count(*) FROM tax_master WHERE name = 'STATE TAX'`); n != 1 {
		t.Errorf("STATE TAX appears %d times in tax_master, want once", n)
	}
	if first.Taxes[0].Name != "STATE TAX" || second.Taxes[0].Name != "STATE TAX" {
		t.Errorf("tax names = %s, %s; want STATE TAX", first.Taxes[0].Name, second.Taxes[0].Name)
	}
	names, err := s.TaxNames(ctx)
	if err != nil || !slices.Contains(names, "STATE TAX") {
		t.Errorf("TaxNames = %v, %v; want it to include STATE TAX", names, err)
	}
}

// TestCreateRefusesUncheckedReceipt: CreateFromReceipt runs after the guards,
// but refuses a receipt they would have rejected instead of crashing.
func TestCreateRefusesUncheckedReceipt(t *testing.T) {
	s, d := setup(t)
	for _, text := range []string{"Espresso 3.50", "Espresso 3.50\nVAT 0.56\nTOTAL 4.06"} {
		receiptID := newReceipt(t, d)
		if _, err := s.CreateFromReceipt(ctx, d, receiptID, testParser.Parse(text)); err == nil {
			t.Errorf("%q: CreateFromReceipt succeeded, want an error", text)
		}
	}
	if n := count(t, d, `SELECT count(*) FROM expense`); n != 0 {
		t.Errorf("%d expense rows, want 0", n)
	}
}

func TestSoftDeleteAndActiveID(t *testing.T) {
	s, d := setup(t)
	e := create(t, s, d, readFixture(t, "receipt-clean"))

	if id, err := s.ActiveIDForReceipt(ctx, d, e.ReceiptID); err != nil || id != e.ID {
		t.Fatalf("ActiveIDForReceipt = %q, %v; want %s", id, err, e.ID)
	}
	for range 2 { // the second call has nothing left to delete
		if err := s.SoftDeleteActiveForReceipt(ctx, d, e.ReceiptID); err != nil {
			t.Fatalf("SoftDeleteActiveForReceipt: %v", err)
		}
	}
	if id, err := s.ActiveIDForReceipt(ctx, d, e.ReceiptID); err != nil || id != "" {
		t.Errorf("after soft delete: ActiveIDForReceipt = %q, %v; want \"\"", id, err)
	}
	if _, err := s.Get(ctx, e.ID); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("Get of a deleted expense: %v, want core.ErrNotFound", err)
	}
	// The deleted expense's taxes and items stay as they were (ARCHITECTURE.md §2).
	if n := count(t, d, `SELECT count(*) FROM expense_line_item WHERE expense_id = ? AND is_deleted = 0`, e.ID); n != 3 {
		t.Errorf("%d active items under the deleted expense, want 3 untouched", n)
	}
	if n := count(t, d, `SELECT count(*) FROM expense_tax WHERE expense_id = ?`, e.ID); n != 1 {
		t.Errorf("%d taxes under the deleted expense, want 1 untouched", n)
	}
}

func TestGetUnknownID(t *testing.T) {
	s, _ := setup(t)
	if _, err := s.Get(ctx, "no-such-id"); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("Get unknown id: %v, want core.ErrNotFound", err)
	}
}

// fakeSource stands in for ReceiptService in re-itemize.
type fakeSource struct {
	text string
	err  error
}

func (f fakeSource) GetParsedReceipt(ctx context.Context, receiptID string) (parser.ParsedReceipt, error) {
	if f.err != nil {
		return parser.ParsedReceipt{}, f.err
	}
	return testParser.Parse(f.text), nil
}

func TestReitemize(t *testing.T) {
	s, d := setup(t)
	e := create(t, s, d, readFixture(t, "receipt-mismatch")) // NEEDS_REVIEW: 4.00 + 6.00 + 1.90 ≠ 18.50

	// The stored OCR now gives a third line, and the items reconcile.
	s.SetParsedReceiptSource(fakeSource{text: header + "Water 4.00\nSnacks 6.00\nMinibar 6.60\nVAT 19% 1.90\nTOTAL 18.50"})
	got, err := s.Reitemize(ctx, e.ID)
	if err != nil {
		t.Fatalf("Reitemize: %v", err)
	}
	if got.ItemizationStatus != core.StatusComplete || len(got.LineItems) != 3 {
		t.Errorf("after re-itemize: %s; want COMPLETE with 3 items", summary(got))
	}
	for _, item := range got.LineItems {
		if item.ID == e.LineItems[0].ID || item.ID == e.LineItems[1].ID {
			t.Errorf("item %s kept its old id; re-itemize replaces all items", item.Description)
		}
	}
	// Header and taxes are untouched, and there is still one expense.
	if got.Total.StringFixed(2) != "18.50" || got.Merchant != e.Merchant || len(got.Taxes) != 1 || !got.Taxes[0].Amount.Equal(dec("1.90")) {
		t.Errorf("header or taxes changed: %s", summary(got))
	}
	if n := count(t, d, `SELECT count(*) FROM expense WHERE receipt_id = ?`, e.ReceiptID); n != 1 {
		t.Errorf("%d expenses for the receipt, want 1", n)
	}
	if n := count(t, d, `SELECT count(*) FROM expense_line_item WHERE expense_id = ? AND is_deleted = 1`, e.ID); n != 2 {
		t.Errorf("%d soft-deleted items, want the 2 old ones", n)
	}

	// Re-itemize recomputes the status from scratch, so it can go back.
	s.SetParsedReceiptSource(fakeSource{text: readFixture(t, "receipt-mismatch")})
	if got, err = s.Reitemize(ctx, e.ID); err != nil || got.ItemizationStatus != core.StatusNeedsReview {
		t.Errorf("second re-itemize: %v, %v; want NEEDS_REVIEW", err, got.ItemizationStatus)
	}
}

func TestReitemizeErrors(t *testing.T) {
	s, d := setup(t)
	e := create(t, s, d, readFixture(t, "receipt-clean"))

	if _, err := s.Reitemize(ctx, e.ID); err == nil {
		t.Error("no source set: Reitemize succeeded, want an error")
	}

	ocrMissing := errors.New("no active receipt_ocr row")
	s.SetParsedReceiptSource(fakeSource{err: ocrMissing})
	before := summary(mustGet(t, s, e.ID))
	if _, err := s.Reitemize(ctx, e.ID); !errors.Is(err, ocrMissing) {
		t.Errorf("source error: Reitemize = %v, want it wrapped", err)
	}
	if after := summary(mustGet(t, s, e.ID)); after != before {
		t.Errorf("a failed re-itemize wrote:\n%s\nwant\n%s", after, before)
	}

	if err := s.SoftDeleteActiveForReceipt(ctx, d, e.ReceiptID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Reitemize(ctx, e.ID); !errors.Is(err, core.ErrNotFound) {
		t.Errorf("deleted expense: Reitemize = %v, want core.ErrNotFound", err)
	}
}

// keep sends an item back unchanged.
func keep(item core.LineItem) core.ItemInput {
	return core.ItemInput{ID: item.ID, Description: item.Description, Amount: item.Amount}
}

func TestPatchItems(t *testing.T) {
	s, d := setup(t)
	e := create(t, s, d, readFixture(t, "receipt-mismatch"))
	water, snacks := e.LineItems[0], e.LineItems[1]

	// Only Water and Snacks: 4.00 + 6.00 + 1.90 = 11.90, not 18.50. Nothing is written.
	before := mustGet(t, s, e.ID)
	_, err := s.PatchItems(ctx, e.ID, []core.ItemInput{keep(water), keep(snacks)})
	var mismatch *core.MismatchError
	if !errors.As(err, &mismatch) || mismatch.Expected.StringFixed(2) != "18.50" ||
		mismatch.Actual.StringFixed(2) != "11.90" || mismatch.Difference.StringFixed(2) != "6.60" {
		t.Fatalf("PatchItems = %v, want a mismatch of 18.50 / 11.90 / 6.60", err)
	}
	if _, err := s.PatchItems(ctx, e.ID, nil); !errors.As(err, &mismatch) {
		t.Errorf("empty list: %v, want a mismatch", err)
	}
	if after := mustGet(t, s, e.ID); summary(after) != summary(before) || after.UpdatedAt != before.UpdatedAt {
		t.Errorf("a rejected PATCH wrote:\n%s", summary(after))
	}

	// Adding the missing 6.60 reconciles. Water and Snacks keep their ids.
	got, err := s.PatchItems(ctx, e.ID, []core.ItemInput{keep(water), keep(snacks), {Description: "Minibar", Amount: dec("6.60")}})
	if err != nil {
		t.Fatalf("PatchItems: %v", err)
	}
	if got.ItemizationStatus != core.StatusComplete || len(got.LineItems) != 3 ||
		got.LineItems[0].ID != water.ID || got.LineItems[1].ID != snacks.ID || got.LineItems[2].Description != "Minibar" {
		t.Fatalf("after adding Minibar: %s", summary(got))
	}
	minibar := got.LineItems[2]
	if by := queryString(t, d, `SELECT created_by FROM expense_line_item WHERE id = ?`, minibar.ID); by != db.ActorUser {
		t.Errorf("new item created_by = %q, want %q", by, db.ActorUser)
	}
	if by := queryString(t, d, `SELECT updated_by FROM expense WHERE id = ?`, e.ID); by != db.ActorUser {
		t.Errorf("expense updated_by = %q, want %q", by, db.ActorUser)
	}

	// Renaming Snacks replaces its row: new id, and it moves after the unchanged items.
	got, err = s.PatchItems(ctx, e.ID, []core.ItemInput{keep(water), {ID: snacks.ID, Description: "Snacks and chips", Amount: dec("6.00")}, keep(minibar)})
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	if len(got.LineItems) != 3 || got.LineItems[0].ID != water.ID || got.LineItems[1].ID != minibar.ID ||
		got.LineItems[2].Description != "Snacks and chips" || got.LineItems[2].ID == snacks.ID {
		t.Errorf("after the rename: %s", summary(got))
	}
	if deleted := queryString(t, d, `SELECT is_deleted FROM expense_line_item WHERE id = ?`, snacks.ID); deleted != "1" {
		t.Errorf("old Snacks row is_deleted = %s, want 1", deleted)
	}

	// Leaving items out soft-deletes them (a merge into one line).
	got, err = s.PatchItems(ctx, e.ID, []core.ItemInput{keep(water), {Description: "Snacks and minibar", Amount: dec("12.60")}})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	if len(got.LineItems) != 2 || got.LineItems[0].ID != water.ID || got.LineItems[1].Description != "Snacks and minibar" {
		t.Errorf("after the merge: %s", summary(got))
	}

	// Ids that aren't active items of this expense.
	for _, id := range []string{"no-such-id", snacks.ID} {
		_, err := s.PatchItems(ctx, e.ID, []core.ItemInput{{ID: id, Description: "X", Amount: dec("16.60")}})
		if !errors.Is(err, core.ErrUnknownItem) {
			t.Errorf("id %s: %v, want core.ErrUnknownItem", id, err)
		}
	}
}

// TestPatchItemsSplit splits one item into two (ARCHITECTURE.md §3.6). The
// split item's row is soft-deleted, the parts come after the unchanged items,
// and the parts must still add up.
func TestPatchItemsSplit(t *testing.T) {
	s, d := setup(t)
	e := create(t, s, d, readFixture(t, "receipt-clean"))
	espresso, sandwich, water := e.LineItems[0], e.LineItems[1], e.LineItems[2]

	// Bread 5.00 + Cheese 3.00 is 0.90 short of the Sandwich's 8.90. Nothing is written.
	before := summary(mustGet(t, s, e.ID))
	_, err := s.PatchItems(ctx, e.ID, []core.ItemInput{keep(espresso),
		{Description: "Bread", Amount: dec("5.00")}, {Description: "Cheese", Amount: dec("3.00")}, keep(water)})
	var mismatch *core.MismatchError
	if !errors.As(err, &mismatch) || mismatch.Difference.StringFixed(2) != "0.90" {
		t.Fatalf("uneven split: %v, want a mismatch of 0.90", err)
	}
	if after := summary(mustGet(t, s, e.ID)); after != before {
		t.Errorf("a refused split wrote:\n%s", after)
	}

	// Bread 5.00 + Cheese 3.90 = 8.90.
	got, err := s.PatchItems(ctx, e.ID, []core.ItemInput{keep(espresso),
		{Description: "Bread", Amount: dec("5.00")}, {Description: "Cheese", Amount: dec("3.90")}, keep(water)})
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	var names []string
	for _, item := range got.LineItems {
		names = append(names, item.Description)
	}
	if got.ItemizationStatus != core.StatusComplete || !slices.Equal(names, []string{"Espresso", "Mineral water", "Bread", "Cheese"}) ||
		got.LineItems[0].ID != espresso.ID || got.LineItems[1].ID != water.ID {
		t.Errorf("after the split: %s; want Espresso and Mineral water kept, then Bread and Cheese", summary(got))
	}
	if deleted := queryString(t, d, `SELECT is_deleted FROM expense_line_item WHERE id = ?`, sandwich.ID); deleted != "1" {
		t.Errorf("the Sandwich row is_deleted = %s, want 1 (its history is kept)", deleted)
	}
	if n := count(t, d, `SELECT count(*) FROM expense_line_item WHERE expense_id = ?`, e.ID); n != 5 {
		t.Errorf("%d item rows, want 5: the 3 original ones and the 2 parts", n)
	}
}

// TestPatchItemsEditAmounts changes amounts: each edited item gets a new row,
// and a negative amount (a discount) is allowed while the total still matches.
func TestPatchItemsEditAmounts(t *testing.T) {
	s, d := setup(t)
	e := create(t, s, d, readFixture(t, "receipt-clean"))
	espresso, sandwich, water := e.LineItems[0], e.LineItems[1], e.LineItems[2]

	// Move 0.50 from Espresso to Mineral water: 3.00 + 8.90 + 3.10 = 15.00.
	got, err := s.PatchItems(ctx, e.ID, []core.ItemInput{
		{ID: espresso.ID, Description: "Espresso", Amount: dec("3.00")},
		keep(sandwich),
		{ID: water.ID, Description: "Mineral water", Amount: dec("3.10")},
	})
	if err != nil {
		t.Fatalf("edit amounts: %v", err)
	}
	if got.ItemizationStatus != core.StatusComplete || len(got.LineItems) != 3 || got.LineItems[0].ID != sandwich.ID ||
		got.LineItems[1].ID == espresso.ID || !got.LineItems[1].Amount.Equal(dec("3.00")) ||
		got.LineItems[2].ID == water.ID || !got.LineItems[2].Amount.Equal(dec("3.10")) {
		t.Fatalf("after editing amounts: %s; want Sandwich kept, then the two edited items with new ids", summary(got))
	}

	// A discount line: 8.90 + 3.00 + 3.60 - 0.50 = 15.00.
	got, err = s.PatchItems(ctx, e.ID, []core.ItemInput{keep(got.LineItems[0]), keep(got.LineItems[1]),
		{ID: got.LineItems[2].ID, Description: "Mineral water", Amount: dec("3.60")},
		{Description: "Discount", Amount: dec("-0.50")}})
	if err != nil || got.ItemizationStatus != core.StatusComplete || len(got.LineItems) != 4 ||
		!got.LineItems[3].Amount.Equal(dec("-0.50")) {
		t.Errorf("discount line: %v, %s; want COMPLETE with a -0.50 item", err, summary(got))
	}
	if n := count(t, d, `SELECT count(*) FROM expense_line_item WHERE expense_id = ? AND is_deleted = 0`, e.ID); n != 4 {
		t.Errorf("%d active item rows, want 4", n)
	}
}

func TestPatchItemsRejectsInvalidItems(t *testing.T) {
	s, d := setup(t)
	e := create(t, s, d, readFixture(t, "receipt-mismatch"))
	water := e.LineItems[0]
	before := summary(mustGet(t, s, e.ID))

	cases := map[string][]core.ItemInput{
		"three decimals":    {{Description: "Minibar", Amount: dec("16.595")}},
		"zero amount":       {keep(water), {Description: "Free", Amount: dec("0")}, {Description: "Rest", Amount: dec("12.60")}},
		"empty description": {{Description: "", Amount: dec("16.60")}},
		"repeated id":       {keep(water), keep(water), {Description: "Rest", Amount: dec("8.60")}},
	}
	for name, items := range cases {
		if _, err := s.PatchItems(ctx, e.ID, items); !errors.Is(err, core.ErrInvalidItem) {
			t.Errorf("%s: %v, want core.ErrInvalidItem", name, err)
		}
	}
	if after := summary(mustGet(t, s, e.ID)); after != before {
		t.Errorf("an invalid PATCH wrote:\n%s", after)
	}
}

// TestSaveWithStaleExpenseConflicts covers the 409 CONFLICT the API can't
// trigger on purpose: another request changed the expense after it was read.
func TestSaveWithStaleExpenseConflicts(t *testing.T) {
	s, d := setup(t)
	e := create(t, s, d, readFixture(t, "receipt-mismatch"))
	stale := mustGet(t, s, e.ID)

	// Another request changes the expense first.
	items := []core.ItemInput{keep(stale.LineItems[0]), keep(stale.LineItems[1]), {Description: "Minibar", Amount: dec("6.60")}}
	if _, err := s.PatchItems(ctx, e.ID, items); err != nil {
		t.Fatalf("PatchItems: %v", err)
	}
	current := summary(mustGet(t, s, e.ID))

	if _, err := s.savePatch(ctx, stale, items); !errors.Is(err, db.ErrConflict) {
		t.Errorf("savePatch with a stale read: %v, want db.ErrConflict", err)
	}
	if _, err := s.saveReitemize(ctx, stale, nil, core.StatusNeedsReview); !errors.Is(err, db.ErrConflict) {
		t.Errorf("saveReitemize with a stale read: %v, want db.ErrConflict", err)
	}
	if after := summary(mustGet(t, s, e.ID)); after != current {
		t.Errorf("a conflicting save wrote:\n%s\nwant\n%s", after, current)
	}
}

func TestTaxNames(t *testing.T) {
	s, _ := setup(t)
	names, err := s.TaxNames(ctx)
	want := []string{"CESS", "CGST", "GST", "HST", "IGST", "IVA", "MWST", "PST",
		"QST", "SALES TAX", "SGST", "TAX", "TVA", "UST", "UTGST", "VAT"}
	if err != nil || !slices.Equal(names, want) {
		t.Errorf("TaxNames = %v, %v; want the 16 seeded names", names, err)
	}
}
