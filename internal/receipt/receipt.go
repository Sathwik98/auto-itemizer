// Package receipt is the ReceiptService (ARCHITECTURE.md §1). It stores
// uploaded receipts, runs OCR, the guards and the parser on them, and is the
// only code that writes the receipts and receipt_ocr tables.
package receipt

import (
	"errors"

	"auto-itemizer/internal/fileupload"
)

// Receipt statuses (ARCHITECTURE.md §3.7).
const (
	StatusUploaded     = "UPLOADED"
	StatusOCRExtracted = "OCR_EXTRACTED"
	StatusProcessed    = "PROCESSED"
	StatusFailed       = "FAILED"
)

// Receipt is one row of the receipts table.
type Receipt struct {
	ID            string
	FileID        string
	Status        string
	FailureReason string // "" when there is none; a guard's code when FAILED
	CreatedAt     string
	UpdatedAt     string
}

// Details is what GET /receipts/{id} returns (ARCHITECTURE.md §3.3).
type Details struct {
	Receipt
	File      fileupload.FileUpload
	ExpenseID string // "" when the receipt has no active expense
}

var (
	// ErrNotFound means there is no receipt with that id (404).
	ErrNotFound = errors.New("receipt not found")

	// ErrLiveOCRNotConfigured means MOCK_OCR is false and no live OCR
	// provider exists yet (501, ARCHITECTURE.md §1.2). Nothing is written.
	ErrLiveOCRNotConfigured = errors.New("live OCR is not configured")

	// errNoOCR means the receipt has no active OCR text yet.
	errNoOCR = errors.New("receipt has no OCR text")
)
