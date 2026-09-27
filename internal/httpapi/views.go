package httpapi

import (
	"auto-itemizer/internal/expense"
	"auto-itemizer/internal/receipt"
)

// The JSON shapes the API returns (ARCHITECTURE.md §3.3, §3.4). Money is a
// string with two decimals ("15.00"), never a JSON number, so it is exact.

type expenseView struct {
	ID            string         `json:"id"`
	ReceiptID     string         `json:"receipt_id"`
	Merchant      string         `json:"merchant"`
	Date          string         `json:"date"`
	Currency      string         `json:"currency"`
	GrandTotal    string         `json:"grand_total"`
	Taxes         []taxView      `json:"taxes"`
	LineItems     []lineItemView `json:"line_items"`
	ItemizeStatus string         `json:"itemize_status"`
}

type taxView struct {
	Name          string  `json:"name"`
	Rate          string  `json:"rate"`           // exact, as printed: "0.19"
	TaxableAmount *string `json:"taxable_amount"` // null when the receipt doesn't show it
	Amount        string  `json:"amount"`
}

type lineItemView struct {
	ID          string `json:"id"`
	Description string `json:"description"`
	Amount      string `json:"amount"`
}

func newExpenseView(e expense.Expense) expenseView {
	v := expenseView{
		ID:            e.ID,
		ReceiptID:     e.ReceiptID,
		Merchant:      e.Merchant,
		Date:          e.Date,
		Currency:      e.Currency,
		GrandTotal:    e.Total.StringFixed(2),
		Taxes:         []taxView{}, // [] rather than null when there are none
		LineItems:     []lineItemView{},
		ItemizeStatus: e.ItemizationStatus,
	}
	for _, tax := range e.Taxes {
		t := taxView{Name: tax.Name, Rate: tax.Rate.String(), Amount: tax.Amount.StringFixed(2)}
		if tax.TaxableAmount != nil {
			taxable := tax.TaxableAmount.StringFixed(2)
			t.TaxableAmount = &taxable
		}
		v.Taxes = append(v.Taxes, t)
	}
	for _, item := range e.LineItems {
		v.LineItems = append(v.LineItems, lineItemView{ID: item.ID, Description: item.Description, Amount: item.Amount.StringFixed(2)})
	}
	return v
}

type receiptView struct {
	ID            string   `json:"id"`
	Status        string   `json:"status"`
	FailureReason *string  `json:"failure_reason"` // null when there is none
	File          fileView `json:"file"`
	ExpenseID     *string  `json:"expense_id"` // null when there is no active expense
	CreatedAt     string   `json:"created_at"`
	UpdatedAt     string   `json:"updated_at"`
}

type fileView struct {
	FileName    string `json:"file_name"`
	ContentType string `json:"content_type"`
	SizeBytes   int64  `json:"size_bytes"`
}

func newReceiptView(d receipt.Details) receiptView {
	return receiptView{
		ID:            d.ID,
		Status:        d.Status,
		FailureReason: nullable(d.FailureReason),
		File:          fileView{FileName: d.File.FileName, ContentType: d.File.ContentType, SizeBytes: d.File.SizeBytes},
		ExpenseID:     nullable(d.ExpenseID),
		CreatedAt:     d.CreatedAt,
		UpdatedAt:     d.UpdatedAt,
	}
}

type uploadView struct {
	ReceiptID string `json:"receipt_id"`
	Status    string `json:"status"`
}

// nullable returns nil for "", so the JSON shows null instead of "".
func nullable(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}
