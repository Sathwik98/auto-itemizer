package expense

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"auto-itemizer/internal/receipt/parser"

	"github.com/shopspring/decimal"
)

// fixtures is the brief's fixture folder, seen from this package's folder.
var fixtures = filepath.Join("..", "..", "fixtures", "task-a")

var testParser = parser.New([]string{"VAT", "GST", "QST", "TAX", "CGST", "SGST"})

func dec(s string) decimal.Decimal {
	return decimal.RequireFromString(s)
}

func decs(values ...string) []decimal.Decimal {
	out := make([]decimal.Decimal, len(values))
	for i, v := range values {
		out[i] = dec(v)
	}
	return out
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	text, err := os.ReadFile(filepath.Join(fixtures, name+".txt"))
	if err != nil {
		t.Fatal(err)
	}
	return string(text)
}

func TestReconcile(t *testing.T) {
	cases := []struct {
		name  string
		items []string
		taxes []string
		total string
		ok    bool
	}{
		{"exact", []string{"3.50", "8.90", "2.60"}, []string{"2.85"}, "17.85", true},
		{"one cent off passes", []string{"15.01"}, []string{"2.85"}, "17.85", true},
		{"two cents off fails", []string{"15.02"}, []string{"2.85"}, "17.85", false},
		{"no taxes", []string{"12.00"}, nil, "12.00", true},
		{"discount line", []string{"10.00", "-1.00"}, []string{"1.71"}, "10.71", true},
		{"no items", nil, []string{"3.83"}, "24.00", false},
	}
	for _, tc := range cases {
		mismatch := reconcile(decs(tc.items...), decs(tc.taxes...), dec(tc.total))
		if (mismatch == nil) != tc.ok {
			t.Errorf("%s: reconcile ok = %v, want %v (%v)", tc.name, mismatch == nil, tc.ok, mismatch)
		}
	}

	// The numbers of the 409 for receipt-mismatch (ARCHITECTURE.md §3.6).
	m := reconcile(decs("4.00", "6.00"), decs("1.90"), dec("18.50"))
	if m == nil || !m.Expected.Equal(dec("18.50")) || !m.Actual.Equal(dec("11.90")) || !m.Difference.Equal(dec("6.60")) {
		t.Errorf("mismatch = %+v, want expected 18.50, actual 11.90, difference 6.60", m)
	}
}

// TestItemizeMatchesGold runs each fixture through parse and itemize and
// compares the status and items with gold.json. The guards are tested in
// package receipt.
func TestItemizeMatchesGold(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(fixtures, "gold.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golds map[string]struct {
		ItemizeStatus string `json:"itemize_status"`
		LineItems     []struct {
			Description string          `json:"description"`
			Amount      decimal.Decimal `json:"amount"`
		} `json:"line_items"`
	}
	if err := json.Unmarshal(data, &golds); err != nil {
		t.Fatal(err)
	}
	if len(golds) != 3 {
		t.Fatalf("gold.json has %d receipts, want 3", len(golds))
	}

	for name, want := range golds {
		p := testParser.Parse(readFixture(t, name))
		taxes := make([]decimal.Decimal, len(p.Taxes))
		for i, tax := range p.Taxes {
			taxes[i] = tax.Amount
		}
		lines, status := itemize(p.CandidateLines, taxes, *p.Total)

		if status != want.ItemizeStatus {
			t.Errorf("%s: status %s, want %s", name, status, want.ItemizeStatus)
		}
		if len(lines) != len(want.LineItems) {
			t.Errorf("%s: %d items, want %d", name, len(lines), len(want.LineItems))
			continue
		}
		for i, w := range want.LineItems {
			if lines[i].Description != w.Description || !lines[i].Amount.Equal(w.Amount) {
				t.Errorf("%s: item %d = %q %s, want %q %s", name, i, lines[i].Description, lines[i].Amount, w.Description, w.Amount)
			}
		}
	}
}

func TestTaxableAmount(t *testing.T) {
	cases := []struct {
		name string
		text string
		want []string // one per tax; "" means nil
	}{
		{"one tax with a subtotal", readFixture(t, "receipt-clean"), []string{"15.00"}},
		{"one incl. tax", readFixture(t, "receipt-tax-only"), []string{"20.17"}},
		{"one tax, no subtotal, not incl.", "Food 10.00\nVAT 19% 1.90\nTOTAL 11.90", []string{""}},
		{"several taxes share one subtotal", "Poutine 10.00\nSubtotal 10.00\nGST 5% 0.50\nQST 9.975% 1.00\nTOTAL 11.50", []string{"", ""}},
		{"a printed base wins over the subtotal", "Food 10.00\nSubtotal 10.00\nVAT 19%  9.00  1.90\nTOTAL 11.90", []string{"9.00"}},
		{"several taxes with printed bases", "Food 10.00\nDrink 20.00\nVAT 7%  10.00  0.70\nVAT 19%  20.00  3.80\nTOTAL 34.50", []string{"10.00", "20.00"}},
	}
	for _, tc := range cases {
		p := testParser.Parse(tc.text)
		if len(p.Taxes) != len(tc.want) {
			t.Errorf("%s: %d taxes, want %d", tc.name, len(p.Taxes), len(tc.want))
			continue
		}
		for i, want := range tc.want {
			got := taxableAmount(p, p.Taxes[i])
			if want == "" && got != nil {
				t.Errorf("%s: tax %d taxable = %s, want none", tc.name, i, got)
			}
			if want != "" && (got == nil || !got.Equal(dec(want))) {
				t.Errorf("%s: tax %d taxable = %v, want %s", tc.name, i, got, want)
			}
		}
	}
}

func TestFitsMoney(t *testing.T) {
	for value, want := range map[string]bool{"3.50": true, "3.5": true, "-1.00": true, "3.505": false, "0.001": false} {
		if got := fitsMoney(dec(value)); got != want {
			t.Errorf("fitsMoney(%s) = %v, want %v", value, got, want)
		}
	}
}
