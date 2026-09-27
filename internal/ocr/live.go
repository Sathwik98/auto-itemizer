package ocr

import (
	"context"

	fileuploadcore "auto-itemizer/internal/fileupload/core"
)

// LiveProvider is where a real OCR vendor would plug in when MOCK_OCR=false.
// That is out of scope (ARCHITECTURE.md §5), so it always returns
// ErrNotConfigured.
type LiveProvider struct{}

func (LiveProvider) Extract(ctx context.Context, f fileuploadcore.FileUpload) (string, error) {
	return "", ErrNotConfigured
}
