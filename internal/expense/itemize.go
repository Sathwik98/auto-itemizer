package expense

import (
	"auto-itemizer/internal/receipt/parser"

	"github.com/shopspring/decimal"
)

// Pure rules (ARCHITECTURE.md §4): no database, so they are tested directly.

// tolerance is the largest difference reconcile accepts.
var tolerance = decimal.RequireFromString("0.01")

// reconcile checks that the item amounts plus the tax amounts add up to the
// total, within 0.01. Amounts are net only: there is no gross fallback. It
// returns nil when they add up, and the numbers for the 409 when they don't.
func reconcile(items, taxes []decimal.Decimal, total decimal.Decimal) *MismatchError {
	actual := sum(items).Add(sum(taxes))
	difference := total.Sub(actual)
	if difference.Abs().LessThanOrEqual(tolerance) {
		return nil
	}
	return &MismatchError{Expected: total, Actual: actual, Difference: difference}
}

// itemize turns the candidate lines into line items and decides the status.
// The lines are kept as printed: no line is ever added to make the numbers
// work, and the total is never changed.
func itemize(lines []parser.Line, taxes []decimal.Decimal, total decimal.Decimal) ([]parser.Line, string) {
	if len(lines) == 0 {
		return nil, StatusNeedsReview
	}
	amounts := make([]decimal.Decimal, len(lines))
	for i, line := range lines {
		amounts[i] = line.Amount
	}
	if reconcile(amounts, taxes, total) != nil {
		return lines, StatusNeedsReview
	}
	return lines, StatusComplete
}

// taxableAmount is the net amount a tax was charged on, when the receipt
// shows it (ARCHITECTURE.md §2). In order:
//  1. the base printed on the tax line ("VAT 19%  10.00  1.90");
//  2. with exactly one tax line, the subtotal, or for an "incl." tax the
//     total minus the tax;
//  3. otherwise nil: a subtotal shared by several taxes is no single tax's base.
func taxableAmount(p parser.ParsedReceipt, tax parser.Tax) *decimal.Decimal {
	if tax.Base != nil {
		return tax.Base
	}
	if len(p.Taxes) != 1 {
		return nil
	}
	if p.Subtotal != nil {
		return p.Subtotal
	}
	if tax.Inclusive && p.Total != nil {
		net := p.Total.Sub(tax.Amount)
		return &net
	}
	return nil
}

// fitsMoney reports whether amount has at most two decimals, so storing it
// with StringFixed(2) doesn't round it.
func fitsMoney(amount decimal.Decimal) bool {
	return amount.Equal(amount.Round(2))
}

func sum(amounts []decimal.Decimal) decimal.Decimal {
	total := decimal.Zero
	for _, amount := range amounts {
		total = total.Add(amount)
	}
	return total
}

// taxAmounts returns the amount of each tax.
func taxAmounts(taxes []Tax) []decimal.Decimal {
	amounts := make([]decimal.Decimal, len(taxes))
	for i, tax := range taxes {
		amounts[i] = tax.Amount
	}
	return amounts
}
