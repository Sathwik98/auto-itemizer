// The guards (ARCHITECTURE.md §1.3): pure checks that the OCR text is a
// usable receipt. The package documentation is in receipt.go.

package receipt

import (
	"fmt"
	"strings"
	"time"
	"unicode"

	"auto-itemizer/internal/receipt/parser"

	"github.com/shopspring/decimal"
)

// Error codes. A failed guard stores its code in receipts.failure_reason and
// returns it with a 422 (ARCHITECTURE.md §1.3).
const (
	CodeOCRFailed            = "OCR_FAILED" // guard 1, applied by the service around the OCR call
	CodeOCRUnreadable        = "OCR_UNREADABLE"
	CodeNotAReceipt          = "NOT_A_RECEIPT"
	CodeHeaderIncomplete     = "HEADER_INCOMPLETE"
	CodeInvalidReceiptValues = "INVALID_RECEIPT_VALUES"
)

// GuardError is returned when a receipt fails a guard.
type GuardError struct {
	Code    string // stored in receipts.failure_reason
	Message string // human-readable reason, returned in the 422 body
}

func (e *GuardError) Error() string {
	return e.Code + ": " + e.Message
}

// guardFailed builds a GuardError with a formatted message.
func guardFailed(code, format string, args ...any) *GuardError {
	return &GuardError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// minReadableChars is guard 2's threshold.
const minReadableChars = 20

// checkAndParse runs guard 2 on the raw OCR text, parses it with p, then runs
// guards 3 to 5 on the result. The first guard that fails is returned as a
// *GuardError. today is passed in so tests can fix the date.
func checkAndParse(p *parser.Parser, text string, today time.Time) (parser.ParsedReceipt, error) {
	if err := checkReadable(text); err != nil {
		return parser.ParsedReceipt{}, err
	}
	parsed := p.Parse(text)
	if err := checkParsed(parsed, today); err != nil {
		return parser.ParsedReceipt{}, err
	}
	return parsed, nil
}

// checkReadable is guard 2: the text needs at least 20 letters or digits.
func checkReadable(text string) error {
	count := 0
	for _, r := range text {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			count++
		}
	}
	if count < minReadableChars {
		return guardFailed(CodeOCRUnreadable,
			"the text has %d letters or digits; at least %d are needed", count, minReadableChars)
	}
	return nil
}

// checkParsed runs guards 3, 4 and 5, in that order.
func checkParsed(p parser.ParsedReceipt, today time.Time) error {
	// Guard 3: is it a receipt at all?
	if p.Total == nil {
		return guardFailed(CodeNotAReceipt, "no total line (TOTAL, SUMME, GESAMT or AMOUNT DUE) with an amount")
	}
	if err := checkHeader(p); err != nil {
		return err
	}
	return checkValues(p, today)
}

// checkHeader is guard 4: merchant, date and currency were found, and every
// tax line has a rate.
func checkHeader(p parser.ParsedReceipt) error {
	var missing []string
	if p.Merchant == "" {
		missing = append(missing, "merchant")
	}
	if p.Date == "" {
		missing = append(missing, "date")
	}
	if p.Currency == "" {
		missing = append(missing, "currency")
	}
	if len(missing) > 0 {
		return guardFailed(CodeHeaderIncomplete, "missing %s", strings.Join(missing, ", "))
	}
	for _, tax := range p.Taxes {
		if tax.Rate == nil {
			return guardFailed(CodeHeaderIncomplete, "the %s line has no rate", tax.Name)
		}
	}
	return nil
}

// checkValues is guard 5: the values make sense. It runs after guard 4, so
// every tax has a rate.
func checkValues(p parser.ParsedReceipt, today time.Time) error {
	total := *p.Total
	if !total.IsPositive() {
		return guardFailed(CodeInvalidReceiptValues, "total %s must be greater than 0", total.StringFixed(2))
	}

	date, err := time.Parse("2006-01-02", p.Date)
	if err != nil {
		return guardFailed(CodeInvalidReceiptValues, "date %q is not a valid YYYY-MM-DD date", p.Date)
	}
	// The receipt has no time zone. In zones ahead of UTC (up to UTC+14) it
	// can already be tomorrow, so allow one day past today's UTC date.
	latest := utcDate(today).AddDate(0, 0, 1)
	if date.After(latest) {
		return guardFailed(CodeInvalidReceiptValues, "date %s is in the future", p.Date)
	}

	if !currencies[p.Currency] {
		return guardFailed(CodeInvalidReceiptValues, "currency %q is not an ISO 4217 code", p.Currency)
	}

	one := decimal.NewFromInt(1)
	for _, tax := range p.Taxes {
		rate := *tax.Rate
		if rate.IsNegative() || rate.GreaterThanOrEqual(one) {
			return guardFailed(CodeInvalidReceiptValues,
				"%s rate %s%% must be at least 0%% and below 100%%", tax.Name, rate.Shift(2))
		}
		if tax.Amount.IsNegative() || tax.Amount.GreaterThanOrEqual(total) {
			return guardFailed(CodeInvalidReceiptValues,
				"%s amount %s must be at least 0 and below the total %s",
				tax.Name, tax.Amount.StringFixed(2), total.StringFixed(2))
		}
	}
	return nil
}

// utcDate returns t's calendar date in UTC, at midnight.
func utcDate(t time.Time) time.Time {
	y, m, d := t.UTC().Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

// currencies holds the active ISO 4217 codes (List One, 2026) that are used
// for payments. Fund, precious-metal and test codes are left out.
var currencies = setOf(`
	AED AFN ALL AMD AOA ARS AUD AWG AZN
	BAM BBD BDT BHD BIF BMD BND BOB BRL BSD BTN BWP BYN BZD
	CAD CDF CHF CLP CNY COP CRC CUP CVE CZK
	DJF DKK DOP DZD
	EGP ERN ETB EUR
	FJD FKP
	GBP GEL GHS GIP GMD GNF GTQ GYD
	HKD HNL HTG HUF
	IDR ILS INR IQD IRR ISK
	JMD JOD JPY
	KES KGS KHR KMF KPW KRW KWD KYD KZT
	LAK LBP LKR LRD LSL LYD
	MAD MDL MGA MKD MMK MNT MOP MRU MUR MVR MWK MXN MYR MZN
	NAD NGN NIO NOK NPR NZD
	OMR
	PAB PEN PGK PHP PKR PLN PYG
	QAR
	RON RSD RUB RWF
	SAR SBD SCR SDG SEK SGD SHP SLE SOS SRD SSP STN SVC SYP SZL
	THB TJS TMT TND TOP TRY TTD TWD TZS
	UAH UGX USD UYU UZS
	VED VES VND VUV
	WST
	XAF XCD XCG XOF XPF
	YER
	ZAR ZMW ZWG
`)

// setOf turns a space-separated list into a set.
func setOf(list string) map[string]bool {
	set := make(map[string]bool)
	for _, item := range strings.Fields(list) {
		set[item] = true
	}
	return set
}
