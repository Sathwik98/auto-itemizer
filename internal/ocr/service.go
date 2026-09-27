// Package ocr is the OcrService (ARCHITECTURE.md §1): a file goes in and text
// comes out. It stores nothing and knows nothing about receipts, so any other
// kind of document could use it.
package ocr

import (
	"context"
	"errors"
	"fmt"
	"time"

	"auto-itemizer/internal/fileupload"
)

// Provider turns a stored file into raw OCR text. Implementations must stop
// when ctx is cancelled; HTTP clients for real vendors do.
type Provider interface {
	Extract(ctx context.Context, f fileupload.FileUpload) (string, error)
}

// ErrNotConfigured is returned when MOCK_OCR=false: live OCR is out of scope
// (ARCHITECTURE.md §1.2).
var ErrNotConfigured = errors.New("live OCR is not configured")

// Service calls the configured provider with a time limit.
type Service struct {
	provider Provider
	timeout  time.Duration
}

// New returns a Service that uses p and gives each call at most timeout.
func New(p Provider, timeout time.Duration) *Service {
	return &Service{provider: p, timeout: timeout}
}

// Extract returns the raw OCR text of f. It returns an error if the provider
// fails or takes longer than the time limit.
func (s *Service) Extract(ctx context.Context, f fileupload.FileUpload) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	text, err := s.provider.Extract(ctx, f)
	if err != nil {
		return "", fmt.Errorf("extract text from file %s: %w", f.ID, err)
	}
	return text, nil
}
