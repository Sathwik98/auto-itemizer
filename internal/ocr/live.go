package ocr

import (
	"context"

	"auto-itemizer/internal/fileupload"
)

// LiveProvider is where a real OCR vendor would plug in when MOCK_OCR=false.
// That is out of scope (ARCHITECTURE.md §5), so it always returns
// ErrNotConfigured.
type LiveProvider struct{}

func (LiveProvider) Extract(ctx context.Context, f fileupload.FileUpload) (string, error) {
	return "", ErrNotConfigured
}
