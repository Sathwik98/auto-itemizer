// Package core holds the receipt types and rules: the Receipt row, the
// receipt statuses and errors, and the guards (guards.go). The OCR text
// parser is in core/parser. It has no database code (internal/layout checks
// this).
package core

import "errors"

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

var (
	// ErrNotFound means there is no receipt with that id (404).
	ErrNotFound = errors.New("receipt not found")

	// ErrLiveOCRNotConfigured means MOCK_OCR is false and no live OCR
	// provider exists yet (501, ARCHITECTURE.md §1.2). Nothing is written.
	ErrLiveOCRNotConfigured = errors.New("live OCR is not configured")

	// ErrNoOCR means the receipt has no active OCR text yet.
	ErrNoOCR = errors.New("receipt has no OCR text")
)
