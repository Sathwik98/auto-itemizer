package parser

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/shopspring/decimal"
)

// fixtures is the brief's fixture folder, seen from this package's folder.
var fixtures = filepath.Join("..", "..", "..", "..", "fixtures", "task-a")

// testParser uses its own list of tax names. The seeded list in tax_master is
// checked by the db tests.
var testParser = New([]string{"VAT", "GST", "TAX", "MWST", "CGST", "SGST", "IGST"})

// dec turns "3.50" into a decimal.Decimal.
func dec(s string) decimal.Decimal {
	return decimal.RequireFromString(s)
}

// gold mirrors one entry of gold.json. Its numbers are decoded straight into
// decimal.Decimal, so no float is involved.
type gold struct {
	Merchant   string          `json:"merchant"`
	Date       string          `json:"date"`
	Currency   string          `json:"currency"`
	GrandTotal decimal.Decimal `json:"grand_total"`
	Taxes      []struct {
		Name   string          `json:"name"`
		Rate   decimal.Decimal `json:"rate"`
		Amount decimal.Decimal `json:"amount"`
	} `json:"taxes"`
	LineItems []struct {
		Description string          `json:"description"`
		Amount      decimal.Decimal `json:"amount"`
	} `json:"line_items"`
}

func TestParseMatchesGold(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(fixtures, "gold.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golds map[string]gold
	if err := json.Unmarshal(data, &golds); err != nil {
		t.Fatal(err)
	}
	if len(golds) != 3 {
		t.Fatalf("gold.json has %d receipts, want 3", len(golds))
	}

	for name, want := range golds {
		text, err := os.ReadFile(filepath.Join(fixtures, name+".txt"))
		if err != nil {
			t.Fatal(err)
		}
		got := testParser.Parse(string(text))

		if got.Merchant != want.Merchant || got.Date != want.Date || got.Currency != want.Currency {
			t.Errorf("%s: header = %q, %q, %q; want %q, %q, %q", name,
				got.Merchant, got.Date, got.Currency, want.Merchant, want.Date, want.Currency)
		}
		if got.Total == nil || !got.Total.Equal(want.GrandTotal) {
			t.Errorf("%s: total = %v, want %s", name, got.Total, want.GrandTotal)
		}

		if len(got.Taxes) != len(want.Taxes) {
			t.Errorf("%s: %d taxes, want %d", name, len(got.Taxes), len(want.Taxes))
		} else {
			for i, w := range want.Taxes {
				g := got.Taxes[i]
				if g.Name != w.Name || g.Rate == nil || !g.Rate.Equal(w.Rate) || !g.Amount.Equal(w.Amount) {
					t.Errorf("%s: tax %d = %s %v %s, want %s %s %s", name, i,
						g.Name, g.Rate, g.Amount, w.Name, w.Rate, w.Amount)
				}
			}
		}

		if len(got.CandidateLines) != len(want.LineItems) {
			t.Errorf("%s: %d candidate lines, want %d", name, len(got.CandidateLines), len(want.LineItems))
		} else {
			for i, w := range want.LineItems {
				g := got.CandidateLines[i]
				if g.Description != w.Description || !g.Amount.Equal(w.Amount) {
					t.Errorf("%s: line %d = %q %s, want %q %s", name, i,
						g.Description, g.Amount, w.Description, w.Amount)
				}
			}
		}
	}
}

// TestParseSubtotalAndInclusive checks what gold.json doesn't cover: the
// subtotal, and whether the tax is inside the total ("incl. VAT").
func TestParseSubtotalAndInclusive(t *testing.T) {
	cases := []struct {
		fixture   string
		subtotal  string // "" means no subtotal line
		inclusive bool
	}{
		{"receipt-clean", "15.00", false},
		{"receipt-mismatch", "10.00", false},
		{"receipt-tax-only", "", true},
	}
	for _, tc := range cases {
		text, err := os.ReadFile(filepath.Join(fixtures, tc.fixture+".txt"))
		if err != nil {
			t.Fatal(err)
		}
		got := testParser.Parse(string(text))

		if tc.subtotal == "" && got.Subtotal != nil {
			t.Errorf("%s: subtotal = %s, want none", tc.fixture, got.Subtotal)
		}
		if tc.subtotal != "" && (got.Subtotal == nil || !got.Subtotal.Equal(dec(tc.subtotal))) {
			t.Errorf("%s: subtotal = %v, want %s", tc.fixture, got.Subtotal, tc.subtotal)
		}
		if len(got.Taxes) != 1 || got.Taxes[0].Inclusive != tc.inclusive {
			t.Errorf("%s: taxes = %+v, want one tax with Inclusive %v", tc.fixture, got.Taxes, tc.inclusive)
		}
	}
}

func TestParseSummaryLines(t *testing.T) {
	for _, line := range []string{
		"TOTAL        5.00",
		"Total:       5.00",
		"total        5.00",
		"SUMME        5.00",
		"GESAMT       5.00",
		"AMOUNT DUE   5.00",
		"Amount  due: 5.00",
	} {
		got := testParser.Parse(line)
		if got.Total == nil || !got.Total.Equal(dec("5.00")) || len(got.CandidateLines) != 0 {
			t.Errorf("%q: total %v, lines %v; want total 5.00 and no lines", line, got.Total, got.CandidateLines)
		}
	}

	for _, line := range []string{"Subtotal      4.00", "ZWISCHENSUMME 4.00"} {
		got := testParser.Parse(line)
		if got.Subtotal == nil || !got.Subtotal.Equal(dec("4.00")) || got.Total != nil || len(got.CandidateLines) != 0 {
			t.Errorf("%q: subtotal %v, total %v, lines %v; want only subtotal 4.00",
				line, got.Subtotal, got.Total, got.CandidateLines)
		}
	}

	got := testParser.Parse("TOTAL 5.00\nTOTAL 6.00")
	if got.Total == nil || !got.Total.Equal(dec("5.00")) {
		t.Errorf("two totals: total = %v, want the first, 5.00", got.Total)
	}
}

func TestParseTaxLines(t *testing.T) {
	cases := []struct {
		line      string
		name      string
		rate      string // "" means the line prints no rate
		amount    string
		inclusive bool
	}{
		{"VAT 19%                2.85", "VAT", "0.19", "2.85", false},
		{"incl. VAT 19%          3.83", "VAT", "0.19", "3.83", true},
		{"inkl. MwSt 7%          0.70", "MWST", "0.07", "0.70", true},
		{"VAT                    2.85", "VAT", "", "2.85", false},
		{"GST 5.5%               1.10", "GST", "0.055", "1.10", false},
		{"State Tax 6%           0.60", "STATE TAX", "0.06", "0.60", false},
		{"MwSt. 19%              2.85", "MWST", "0.19", "2.85", false},
		{"CGST 9%               45.00", "CGST", "0.09", "45.00", false},
		{"SGST 9%               45.00", "SGST", "0.09", "45.00", false},
		{"IGST 18%              90.00", "IGST", "0.18", "90.00", false},
		{"CGST @ 9%             45.00", "CGST", "0.09", "45.00", false},
		{"VAT\t19%\t2.85", "VAT", "0.19", "2.85", false},
	}
	for _, tc := range cases {
		got := testParser.Parse(tc.line)
		if len(got.Taxes) != 1 || len(got.CandidateLines) != 0 {
			t.Errorf("%q: taxes %+v, lines %v; want one tax and no lines", tc.line, got.Taxes, got.CandidateLines)
			continue
		}
		tax := got.Taxes[0]
		rateOK := (tc.rate == "" && tax.Rate == nil) || (tc.rate != "" && tax.Rate != nil && tax.Rate.Equal(dec(tc.rate)))
		if tax.Name != tc.name || !rateOK || !tax.Amount.Equal(dec(tc.amount)) || tax.Inclusive != tc.inclusive {
			t.Errorf("%q: got %s %v %s incl=%v, want %s %q %s incl=%v", tc.line,
				tax.Name, tax.Rate, tax.Amount, tax.Inclusive, tc.name, tc.rate, tc.amount, tc.inclusive)
		}
	}
}

// TestParseTaxBase covers tax lines that print the base before the tax
// amount, as in a tax table: "VAT 19%  10.00  1.90".
func TestParseTaxBase(t *testing.T) {
	cases := []struct {
		line      string
		name      string
		base      string
		amount    string
		inclusive bool
	}{
		{"VAT 19%          10.00      1.90", "VAT", "10.00", "1.90", false},
		{"incl. VAT 19%    20.17      3.83", "VAT", "20.17", "3.83", true},
		{"CGST @ 9%       500.00     45.00", "CGST", "500.00", "45.00", false},
	}
	for _, tc := range cases {
		got := testParser.Parse(tc.line)
		if len(got.Taxes) != 1 || len(got.CandidateLines) != 0 {
			t.Errorf("%q: taxes %+v, lines %v; want one tax and no lines", tc.line, got.Taxes, got.CandidateLines)
			continue
		}
		tax := got.Taxes[0]
		if tax.Name != tc.name || tax.Base == nil || !tax.Base.Equal(dec(tc.base)) ||
			!tax.Amount.Equal(dec(tc.amount)) || tax.Inclusive != tc.inclusive {
			t.Errorf("%q: got %s base %v amount %s incl=%v, want %s base %s amount %s incl=%v", tc.line,
				tax.Name, tax.Base, tax.Amount, tax.Inclusive, tc.name, tc.base, tc.amount, tc.inclusive)
		}
	}

	// A tax line without a base keeps Base nil.
	if got := testParser.Parse("VAT 19%    2.85"); len(got.Taxes) != 1 || got.Taxes[0].Base != nil {
		t.Errorf("VAT without a base: taxes %+v, want Base nil", got.Taxes)
	}

	// A quantity line has two amounts too, but "2 x" isn't a tax label.
	got := testParser.Parse("2 x 3.50           7.00")
	if len(got.Taxes) != 0 || len(got.CandidateLines) != 1 || got.CandidateLines[0].Description != "2 x 3.50" {
		t.Errorf("quantity line: taxes %+v, lines %v; want the item \"2 x 3.50\"", got.Taxes, got.CandidateLines)
	}
}

func TestParseItemLines(t *testing.T) {
	cases := []struct {
		line        string
		description string
		amount      string
	}{
		{"Espresso                    3.50", "Espresso", "3.50"},
		{"Espresso\t3.50", "Espresso", "3.50"},
		{"Rabatt                     -1.00", "Rabatt", "-1.00"},
		// A rate alone doesn't make a tax: the name must contain a tax name.
		{"Discount 10%               -1.00", "Discount 10%", "-1.00"},
		{"Service charge 10%          1.50", "Service charge 10%", "1.50"},
		// A known limit: "KURTAXE" isn't a tax name, and doesn't contain one
		// as a whole word, so this tax is read as an item.
		{"Kurtaxe 3%                  1.50", "Kurtaxe 3%", "1.50"},
	}
	for _, tc := range cases {
		got := testParser.Parse(tc.line)
		if len(got.CandidateLines) != 1 || len(got.Taxes) != 0 {
			t.Errorf("%q: lines %v, taxes %+v; want one line and no taxes", tc.line, got.CandidateLines, got.Taxes)
			continue
		}
		line := got.CandidateLines[0]
		if line.Description != tc.description || !line.Amount.Equal(dec(tc.amount)) {
			t.Errorf("%q: got %q %s, want %q %s", tc.line, line.Description, line.Amount, tc.description, tc.amount)
		}
	}
}

func TestParseIgnoredLines(t *testing.T) {
	for _, line := range []string{
		"Trip fare",
		"(No itemized list)",
		"Water                       0.00",
		"3.50",          // an amount with no label
		"Espresso 3.5",  // one decimal: not read as an amount
		"Espresso 3,50", // comma decimals are out of scope
	} {
		got := testParser.Parse(line)
		if len(got.CandidateLines) != 0 || len(got.Taxes) != 0 || got.Total != nil || got.Subtotal != nil {
			t.Errorf("%q: parsed %+v, want nothing", line, got)
		}
	}
}

func TestParseTakesItemsOnlyAboveTheTotal(t *testing.T) {
	got := testParser.Parse(`Espresso      3.50
TOTAL         3.50
Cash         20.00
Change      -16.50
Card          3.50
VAT 19%       0.56`)
	if len(got.CandidateLines) != 1 || got.CandidateLines[0].Description != "Espresso" {
		t.Errorf("lines = %v, want only Espresso: payment lines under the total are not items", got.CandidateLines)
	}
	if len(got.Taxes) != 1 || got.Taxes[0].Name != "VAT" {
		t.Errorf("taxes = %+v, want the VAT printed under the total", got.Taxes)
	}

	// A receipt that prints its total first loses its items. Reconciliation
	// then fails, so the expense gets NEEDS_REVIEW instead of wrong data.
	got = testParser.Parse("TOTAL 12.40\nEspresso 3.50\nSandwich 8.90")
	if len(got.CandidateLines) != 0 || got.Total == nil || !got.Total.Equal(dec("12.40")) {
		t.Errorf("total first: lines %v, total %v; want no lines and total 12.40", got.CandidateLines, got.Total)
	}
}

func TestParseHeader(t *testing.T) {
	// Labels in any case, the first value wins, Windows line endings.
	got := testParser.Parse("merchant: Cafe Mitte\r\nMERCHANT: Other Shop\r\nDate: 2026-03-12\r\ncurrency: eur\r\nTOTAL 1.00\r\n")
	if got.Merchant != "Cafe Mitte" || got.Date != "2026-03-12" || got.Currency != "EUR" {
		t.Errorf("header = %q, %q, %q; want Cafe Mitte, 2026-03-12, EUR", got.Merchant, got.Date, got.Currency)
	}
	if got.Total == nil || !got.Total.Equal(dec("1.00")) {
		t.Errorf("total = %v, want 1.00", got.Total)
	}

	got = testParser.Parse("TOTAL 1.00")
	if got.Merchant != "" || got.Date != "" || got.Currency != "" {
		t.Errorf("no header lines: got %q, %q, %q; want empty", got.Merchant, got.Date, got.Currency)
	}
}

// TestTaxNamesDriveParsing shows that the list of names decides what is a
// tax: in the service, adding a row to tax_master (and restarting) is enough.
func TestTaxNamesDriveParsing(t *testing.T) {
	line := "Kurtaxe 3%      1.50"

	got := New([]string{"VAT"}).Parse(line)
	if len(got.Taxes) != 0 || len(got.CandidateLines) != 1 {
		t.Errorf("without KURTAXE: taxes %+v, lines %v; want an item", got.Taxes, got.CandidateLines)
	}

	got = New([]string{"VAT", "Kurtaxe"}).Parse(line)
	if len(got.Taxes) != 1 || got.Taxes[0].Name != "KURTAXE" || len(got.CandidateLines) != 0 {
		t.Errorf("with Kurtaxe: taxes %+v, lines %v; want the tax KURTAXE", got.Taxes, got.CandidateLines)
	}
}
