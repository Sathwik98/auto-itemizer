// Package expense is the ExpenseService (ARCHITECTURE.md §1): the expense,
// which is the brief's "transaction", with its taxes and line items. Itemize
// and reconcile are internal steps here (§4).
package expense

import (
	"context"
	"errors"
	"fmt"

	"auto-itemizer/internal/receipt/parser"

	"github.com/shopspring/decimal"
)

// Itemization statuses (ARCHITECTURE.md §4), returned as itemize_status.
const (
	StatusComplete    = "COMPLETE"
	StatusNeedsReview = "NEEDS_REVIEW"

	// StatusFailed is reserved for a parser that can fail on the items, such
	// as a live OCR provider. The stub parser never fails, so this build
	// never sets it.
	StatusFailed = "FAILED"
)

// Expense is one expense with its taxes and its active line items.
type Expense struct {
	ID                string
	ReceiptID         string
	Merchant          string
	Date              string
	Currency          string
	Total             decimal.Decimal
	ItemizationStatus string
	Taxes             []Tax
	LineItems         []LineItem // active items, in the order they were inserted
	UpdatedAt         string     // read before a change, for the conflict check
}

// Tax is one expense_tax row with its tax_master name.
type Tax struct {
	Name          string
	Rate          decimal.Decimal
	TaxableAmount *decimal.Decimal // nil when the receipt doesn't show it
	Amount        decimal.Decimal
}

// LineItem is one active expense_line_item row.
type LineItem struct {
	ID          string
	Description string
	Amount      decimal.Decimal
}

// ItemInput is one item of a PATCH body. ID is "" for a new item.
type ItemInput struct {
	ID          string
	Description string
	Amount      decimal.Decimal
}

// ParsedReceiptSource gives re-itemize a receipt's stored OCR text, parsed.
// ReceiptService implements it (IMPLEMENTATION_PLAN.md §3).
type ParsedReceiptSource interface {
	GetParsedReceipt(ctx context.Context, receiptID string) (parser.ParsedReceipt, error)
}

var (
	// ErrNotFound means there is no active expense with that id (404).
	ErrNotFound = errors.New("expense not found")

	// ErrUnknownItem means a PATCH names an id that isn't an active item of
	// the expense (400).
	ErrUnknownItem = errors.New("not an active item of this expense")

	// ErrInvalidItem means a PATCH item breaks a rule of ARCHITECTURE.md §3.6:
	// an empty description, a zero amount, more than two decimals, or an id
	// that appears twice (400). The controller checks these too; the service
	// checks again so no caller can store a rounded or duplicated item.
	ErrInvalidItem = errors.New("invalid item")
)

// MismatchError means the items plus the taxes don't add up to the total
// (409 ITEMS_DO_NOT_RECONCILE, ARCHITECTURE.md §3.6).
type MismatchError struct {
	Expected   decimal.Decimal // the total
	Actual     decimal.Decimal // items plus taxes
	Difference decimal.Decimal // expected minus actual
}

func (e *MismatchError) Error() string {
	return fmt.Sprintf("items plus taxes are %s but the total is %s (difference %s)",
		e.Actual.StringFixed(2), e.Expected.StringFixed(2), e.Difference.StringFixed(2))
}
