package core

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"auto-itemizer/internal/receipt/core/parser"

	"github.com/shopspring/decimal"
)

// today is fixed, so the date tests don't depend on when they run.
var today = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

var testParser = parser.New([]string{"VAT", "GST", "TAX"})

// codeOf returns the guard code of err, or "" when err is nil. Any other kind
// of error fails the test.
func codeOf(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	var guardErr *GuardError
	if !errors.As(err, &guardErr) {
		t.Fatalf("error %v is not a *GuardError", err)
	}
	if guardErr.Message == "" {
		t.Errorf("%s has no message", guardErr.Code)
	}
	return guardErr.Code
}

func TestFixtures(t *testing.T) {
	cases := map[string]string{ // file under fixtures/ → expected code
		"task-a/receipt-clean.txt":       "",
		"task-a/receipt-tax-only.txt":    "",
		"task-a/receipt-mismatch.txt":    "",
		"mock-ocr/unreadable.txt":        CodeOCRUnreadable,
		"mock-ocr/not-a-receipt.txt":     CodeNotAReceipt,
		"mock-ocr/header-incomplete.txt": CodeHeaderIncomplete,
		"mock-ocr/invalid-values.txt":    CodeInvalidReceiptValues,
	}
	for file, want := range cases {
		text, err := os.ReadFile(filepath.Join("..", "..", "..", "fixtures", file))
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := CheckAndParse(testParser, string(text), today)
		if got := codeOf(t, err); got != want {
			t.Errorf("%s: code %q, want %q (%v)", file, got, want, err)
		}
		if want == "" && parsed.Merchant == "" {
			t.Errorf("%s: passed but returned an empty parsed receipt", file)
		}
	}
}

// validReceipt passes every guard. Most cases below change one part of it.
const validReceipt = `MERCHANT: Cafe Mitte
DATE: 2026-03-12
CURRENCY: EUR

Espresso                    3.50
VAT 19%                     0.56
TOTAL                       4.06`

// withChanges returns validReceipt with each old string replaced by the new
// one after it. It panics when old isn't there, so a typo can't quietly turn
// a case into a copy of the valid receipt.
func withChanges(oldNew ...string) string {
	text := validReceipt
	for i := 0; i < len(oldNew); i += 2 {
		if !strings.Contains(text, oldNew[i]) {
			panic("validReceipt has no " + oldNew[i])
		}
		text = strings.Replace(text, oldNew[i], oldNew[i+1], 1)
	}
	return text
}

func TestGuards(t *testing.T) {
	cases := []struct {
		name string
		text string
		want string // "" means every guard passes
	}{
		{"valid receipt", validReceipt, ""},

		// Guard 2: at least 20 letters or digits.
		{"19 letters or digits", "abcdefghij123456789", CodeOCRUnreadable},
		{"20 letters or digits", "abcdefghij1234567890", CodeNotAReceipt}, // passes guard 2, fails 3

		// Guard 3: a total line with an amount.
		{"no total", withChanges("TOTAL                       4.06", ""), CodeNotAReceipt},

		// Guard 4: merchant, date, currency, and a rate on every tax.
		{"no merchant", withChanges("MERCHANT: Cafe Mitte\n", ""), CodeHeaderIncomplete},
		{"no date", withChanges("DATE: 2026-03-12\n", ""), CodeHeaderIncomplete},
		{"no currency", withChanges("CURRENCY: EUR\n", ""), CodeHeaderIncomplete},
		{"tax without a rate", withChanges("VAT 19%", "VAT"), CodeHeaderIncomplete},

		// Guard 5: the values make sense.
		{"total 0.00", withChanges("4.06", "0.00"), CodeInvalidReceiptValues},
		{"negative total", withChanges("4.06", "-1.00"), CodeInvalidReceiptValues},
		{"impossible date", withChanges("2026-03-12", "2026-02-30"), CodeInvalidReceiptValues},
		{"non-ISO date", withChanges("2026-03-12", "12.03.2026"), CodeInvalidReceiptValues},
		{"dated today", withChanges("2026-03-12", "2026-09-27"), ""},
		{"dated tomorrow", withChanges("2026-03-12", "2026-09-28"), ""}, // ahead-of-UTC time zones
		{"dated the day after", withChanges("2026-03-12", "2026-09-29"), CodeInvalidReceiptValues},
		{"unknown currency", withChanges("EUR", "EURO"), CodeInvalidReceiptValues},
		{"no-currency code XXX", withChanges("EUR", "XXX"), CodeInvalidReceiptValues},
		{"lower-case currency", withChanges("EUR", "eur"), ""},
		{"rate 0%", withChanges("VAT 19%", "VAT 0%"), ""},
		{"rate 100%", withChanges("VAT 19%", "VAT 100%"), CodeInvalidReceiptValues},
		{"tax equal to the total", withChanges("0.56", "4.06"), CodeInvalidReceiptValues},
		{"negative tax", withChanges("0.56", "-0.56"), CodeInvalidReceiptValues},

		// Guards run in order: guard 4 is reported before guard 5.
		{"fails guards 4 and 5", withChanges("MERCHANT: Cafe Mitte\n", "", "4.06", "0.00"), CodeHeaderIncomplete},
	}
	for _, tc := range cases {
		_, err := CheckAndParse(testParser, tc.text, today)
		if got := codeOf(t, err); got != tc.want {
			t.Errorf("%s: code %q, want %q (%v)", tc.name, got, tc.want, err)
		}
	}
}

// TestNegativeRate covers a value the text parser can't produce: its rate
// pattern has no minus sign. A future parser might, so guard 5 checks it.
func TestNegativeRate(t *testing.T) {
	parsed := testParser.Parse(validReceipt)
	negative := decimal.RequireFromString("-0.19")
	parsed.Taxes[0].Rate = &negative

	if got := codeOf(t, checkParsed(parsed, today)); got != CodeInvalidReceiptValues {
		t.Errorf("negative rate: code %q, want %s", got, CodeInvalidReceiptValues)
	}
}
