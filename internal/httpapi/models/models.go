// Package models holds the JSON request and response bodies of the API
// (ARCHITECTURE.md §3). They are kept apart from the domain types in each
// feature's core on purpose: in JSON, money is a string with two decimals
// ("15.00", never a JSON number, so it is exact), and a missing value is null.
package models

import (
	expensecore "auto-itemizer/internal/expense/core"
	fileuploadcore "auto-itemizer/internal/fileupload/core"
	receiptcore "auto-itemizer/internal/receipt/core"

	"github.com/shopspring/decimal"
)

// ExpenseResponse is an expense with its taxes and line items (§3.4): the
// body of GET /transactions/{id}, and of process, itemize and PATCH.
type ExpenseResponse struct {
	ID            string             `json:"id"`
	ReceiptID     string             `json:"receipt_id"`
	Merchant      string             `json:"merchant"`
	Date          string             `json:"date"`
	Currency      string             `json:"currency"`
	GrandTotal    string             `json:"grand_total"`
	Taxes         []TaxResponse      `json:"taxes"`
	LineItems     []LineItemResponse `json:"line_items"`
	ItemizeStatus string             `json:"itemize_status"`
}

// TaxResponse is one tax of an ExpenseResponse.
type TaxResponse struct {
	Name          string  `json:"name"`
	Rate          string  `json:"rate"`           // exact, as printed: "0.19"
	TaxableAmount *string `json:"taxable_amount"` // null when the receipt doesn't show it
	Amount        string  `json:"amount"`
}

// LineItemResponse is one line item of an ExpenseResponse.
type LineItemResponse struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Amount      string `json:"amount"`
}

// NewExpenseResponse builds the response for e.
func NewExpenseResponse(e expensecore.Expense) ExpenseResponse {
	v := ExpenseResponse{
		ID:            e.ID,
		ReceiptID:     e.ReceiptID,
		Merchant:      e.Merchant,
		Date:          e.Date,
		Currency:      e.Currency,
		GrandTotal:    e.Total.StringFixed(2),
		Taxes:         []TaxResponse{}, // [] rather than null when there are none
		LineItems:     []LineItemResponse{},
		ItemizeStatus: e.ItemizationStatus,
	}
	for _, tax := range e.Taxes {
		t := TaxResponse{Name: tax.Name, Rate: tax.Rate.String(), Amount: tax.Amount.StringFixed(2)}
		if tax.TaxableAmount != nil {
			taxable := tax.TaxableAmount.StringFixed(2)
			t.TaxableAmount = &taxable
		}
		v.Taxes = append(v.Taxes, t)
	}
	for _, item := range e.LineItems {
		v.LineItems = append(v.LineItems, LineItemResponse{ID: item.ID, Description: item.Description, Amount: item.Amount.StringFixed(2)})
	}
	return v
}

// ReceiptResponse is the body of GET /receipts/{id} (§3.3).
type ReceiptResponse struct {
	ID            string       `json:"id"`
	Status        string       `json:"status"`
	FailureReason *string      `json:"failure_reason"` // null when there is none
	File          FileResponse `json:"file"`
	ExpenseID     *string      `json:"expense_id"` // null when there is no active expense
	CreatedAt     string       `json:"created_at"`
	UpdatedAt     string       `json:"updated_at"`
}

// FileResponse is the uploaded file of a ReceiptResponse.
type FileResponse struct {
	FileName    string `json:"file_name"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
}

// NewReceiptResponse builds the response for receipt r, its file f and the id
// of its active expense ("" when it has none).
func NewReceiptResponse(r receiptcore.Receipt, f fileuploadcore.FileUpload, expenseID string) ReceiptResponse {
	return ReceiptResponse{
		ID:            r.ID,
		Status:        r.Status,
		FailureReason: nullable(r.FailureReason),
		File:          FileResponse{FileName: f.FileName, ContentType: f.ContentType, SizeBytes: f.SizeBytes},
		ExpenseID:     nullable(expenseID),
		CreatedAt:     r.CreatedAt,
		UpdatedAt:     r.UpdatedAt,
	}
}

// UploadResponse is the body of POST /receipts (§3.1).
type UploadResponse struct {
	ReceiptID string `json:"receipt_id"`
	Status    string `json:"status"`
}

// ErrorResponse is the body of every 4xx and 5xx response (§3.0). The last
// three fields are only set for ITEMS_DO_NOT_RECONCILE.
type ErrorResponse struct {
	Error      string `json:"error"`
	Message    string `json:"message"`
	Expected   string `json:"expected,omitempty"`
	Actual     string `json:"actual,omitempty"`
	Difference string `json:"difference,omitempty"`
}

// ItemRequest is one item of a PATCH /transactions/{id}/items body (§3.6).
// The amount may be sent as "6.60" or 6.60; decimal.Decimal reads both
// exactly.
type ItemRequest struct {
	ID          string          `json:"id"`
	Description string          `json:"description"`
	Amount      decimal.Decimal `json:"amount"`
}

// nullable returns nil for "", so the JSON shows null instead of "".
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
