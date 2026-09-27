// Package parser reads receipt OCR text into a ParsedReceipt
// (ARCHITECTURE.md §1). It does no I/O, so it can be tested on its own. It is
// a separate package so that both receipt and expense can import it
// (IMPLEMENTATION_PLAN.md §3).
package parser

import (
	"regexp"
	"strings"
	"unicode"

	"github.com/shopspring/decimal"
)

// ParsedReceipt is what Parse reads from OCR text. Anything the text doesn't
// contain is left empty ("" or nil), so the guards can say what's missing.
type ParsedReceipt struct {
	Merchant       string
	Date           string           // as printed; guard 5 checks it is a real YYYY-MM-DD date
	Currency       string           // as printed, upper-cased
	Total          *decimal.Decimal // nil: no total line
	Subtotal       *decimal.Decimal // nil: no subtotal line
	Taxes          []Tax
	CandidateLines []Line // lines itemize may use (ARCHITECTURE.md §4), in printed order
}

// Tax is one printed tax line, such as "VAT 19%   2.85".
type Tax struct {
	Name      string           // upper-cased, e.g. "VAT"
	Rate      *decimal.Decimal // 0.19 for "19%"; nil when the line prints no rate
	Base      *decimal.Decimal // the net amount taxed, when printed ("VAT 19%  10.00  1.90"); nil otherwise
	Amount    decimal.Decimal
	Inclusive bool // "incl. VAT …": the tax is already inside the total
}

// Line is a candidate line item: a description and a non-zero amount.
type Line struct {
	Description string
	Amount      decimal.Decimal
}

// Parser recognises tax lines using the tax names it was built with. In the
// running service the names come from tax_master, loaded once at startup.
type Parser struct {
	taxNames map[string]bool // cleaned by taxName, e.g. "VAT", "CGST", "SALES TAX"
}

// New returns a Parser that treats the given names as tax names.
func New(taxNames []string) *Parser {
	names := make(map[string]bool, len(taxNames))
	for _, name := range taxNames {
		names[taxName(name)] = true
	}
	return &Parser{taxNames: names}
}

// The patterns are compiled once, when the program starts.
var (
	// headerLine matches "MERCHANT: Cafe Mitte", with the label in any case.
	headerLine = regexp.MustCompile(`(?i)^(MERCHANT|DATE|CURRENCY)\s*:\s*(.*)$`)

	// amountLine splits "Espresso    3.50" into a label and an amount: the
	// amount has two decimals, an optional minus sign, and ends the line.
	amountLine = regexp.MustCompile(`^(.*\S)\s+(-?\d+\.\d{2})$`)

	// inclusivePrefix matches "incl." or "inkl." at the start of a tax label.
	inclusivePrefix = regexp.MustCompile(`(?i)^(incl|inkl)\.?\s+`)

	// ratePart matches the rate at the end of a tax label: "19%" or "5.5 %".
	ratePart = regexp.MustCompile(`(\d+(?:\.\d+)?)\s*%$`)
)

// Labels of the summary lines, upper-cased with single spaces.
var (
	totalLabels    = map[string]bool{"TOTAL": true, "SUMME": true, "GESAMT": true, "AMOUNT DUE": true}
	subtotalLabels = map[string]bool{"SUBTOTAL": true, "ZWISCHENSUMME": true}
)

// Parse reads the receipt one line at a time. Each line goes to the first of
// three small parsers that recognises it: header lines, summary lines (total,
// subtotal and tax) and item lines. Header and summary lines count wherever
// they are printed. Item lines count only above the first total line, so
// payment lines under the total ("Cash 20.00") never become items.
func (p *Parser) Parse(text string) ParsedReceipt {
	var r ParsedReceipt
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line) // also removes the \r of Windows line endings
		if line == "" {
			continue
		}
		if parseHeaderLine(&r, line) {
			continue
		}
		label, amount, ok := splitAmount(line)
		if !ok {
			continue // no amount, e.g. "Trip fare" or "(No itemized list)"
		}
		if p.parseSummaryLine(&r, label, amount) {
			continue
		}
		if r.Total == nil {
			parseItemLine(&r, label, amount)
		}
	}
	return r
}

// parseHeaderLine reads "MERCHANT: …", "DATE: …" and "CURRENCY: …". If a
// label appears twice, the first value is kept.
func parseHeaderLine(r *ParsedReceipt, line string) bool {
	m := headerLine.FindStringSubmatch(line)
	if m == nil {
		return false
	}
	value := strings.TrimSpace(m[2])
	switch strings.ToUpper(m[1]) {
	case "MERCHANT":
		if r.Merchant == "" {
			r.Merchant = value
		}
	case "DATE":
		if r.Date == "" {
			r.Date = value
		}
	case "CURRENCY":
		if r.Currency == "" {
			r.Currency = strings.ToUpper(value)
		}
	}
	return true
}

// splitAmount splits "Espresso    3.50" into "Espresso" and 3.50. ok is false
// when the line doesn't end in an amount, or has no label before it.
func splitAmount(line string) (label string, amount decimal.Decimal, ok bool) {
	m := amountLine.FindStringSubmatch(line)
	if m == nil {
		return "", decimal.Decimal{}, false
	}
	// The pattern only matches digits with two decimals, so this can't fail.
	return m[1], decimal.RequireFromString(m[2]), true
}

// parseSummaryLine reads total, subtotal and tax lines. Only the first total
// and the first subtotal count.
func (p *Parser) parseSummaryLine(r *ParsedReceipt, label string, amount decimal.Decimal) bool {
	key := strings.Join(strings.Fields(strings.TrimSuffix(strings.ToUpper(label), ":")), " ")
	if totalLabels[key] {
		if r.Total == nil {
			r.Total = &amount
		}
		return true
	}
	if subtotalLabels[key] {
		if r.Subtotal == nil {
			r.Subtotal = &amount
		}
		return true
	}
	tax, ok := p.parseTax(label, amount)
	if ok {
		r.Taxes = append(r.Taxes, tax)
	}
	return ok
}

// parseTax reads a tax line. Some receipts print the base before the tax
// amount: in "VAT 19%  10.00  1.90" the label is "VAT 19%  10.00". If the
// label isn't a tax label as it stands, parseTax strips a trailing amount and
// tries again, keeping that amount as the base. "2 x 3.50  7.00" stays an
// item, because "2 x" isn't a tax label.
func (p *Parser) parseTax(label string, amount decimal.Decimal) (Tax, bool) {
	if tax, ok := p.parseTaxLabel(label, amount); ok {
		return tax, true
	}
	rest, base, ok := splitAmount(label)
	if !ok {
		return Tax{}, false
	}
	tax, ok := p.parseTaxLabel(rest, amount)
	if !ok {
		return Tax{}, false
	}
	tax.Base = &base
	return tax, true
}

// parseTaxLabel reads a tax label such as "VAT 19%", "incl. VAT 19%" or just
// "VAT". A label with a rate is a tax when it contains one of the tax names
// as whole words ("State Tax 6%" contains TAX). A label without a rate must be
// exactly a tax name. So "Discount 10%" and "Service charge 10%" stay items.
func (p *Parser) parseTaxLabel(label string, amount decimal.Decimal) (Tax, bool) {
	tax := Tax{Amount: amount}
	rest := label
	if prefix := inclusivePrefix.FindString(rest); prefix != "" {
		tax.Inclusive = true
		rest = rest[len(prefix):]
	}
	if m := ratePart.FindStringSubmatch(rest); m != nil {
		rate := decimal.RequireFromString(m[1]).Shift(-2) // "19" → 0.19, exact
		tax.Rate = &rate
		rest = rest[:len(rest)-len(m[0])]
	}
	tax.Name = taxName(rest)

	if tax.Rate != nil && p.containsTaxName(tax.Name) {
		return tax, true
	}
	if tax.Rate == nil && p.taxNames[tax.Name] {
		return tax, true
	}
	return Tax{}, false
}

// containsTaxName reports whether name contains one of the tax names as whole
// words: "STATE TAX" contains "TAX", but "KURTAXE" doesn't.
func (p *Parser) containsTaxName(name string) bool {
	words := " " + strings.Join(wordsOf(name), " ") + " "
	for known := range p.taxNames {
		if strings.Contains(words, " "+known+" ") {
			return true
		}
	}
	return false
}

// parseItemLine keeps a line with a non-zero amount as a candidate line item.
// Negative amounts are discounts, so they count too.
func parseItemLine(r *ParsedReceipt, label string, amount decimal.Decimal) {
	if amount.IsZero() {
		return
	}
	r.CandidateLines = append(r.CandidateLines, Line{Description: label, Amount: amount})
}

// taxName tidies a tax name: upper case, single spaces and no trailing "." or
// "@". "MwSt." becomes "MWST" and "CGST @" becomes "CGST".
func taxName(printed string) string {
	name := strings.Join(strings.Fields(strings.ToUpper(printed)), " ")
	return strings.TrimRight(name, " .@")
}

// wordsOf splits s into its runs of letters and digits.
func wordsOf(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}
