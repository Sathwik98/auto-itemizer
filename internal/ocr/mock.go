package ocr

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"auto-itemizer/internal/fileupload"
)

// MockProvider is used when MOCK_OCR=true (ARCHITECTURE.md §1.2). It returns
// the fixture text whose name matches the uploaded file's name, or the
// fallback text when none does. It never reads the stored file.
type MockProvider struct {
	texts        map[string]string // fixture name without .txt → its text
	fallback     string
	fallbackPath string
}

// NewMockProvider loads every .txt file in fixtureDirs, and the fallback file,
// into memory. A missing folder or fallback file is an error, so a wrong path
// is caught at startup.
func NewMockProvider(fixtureDirs []string, fallbackPath string) (*MockProvider, error) {
	texts := make(map[string]string)
	for _, dir := range fixtureDirs {
		entries, err := os.ReadDir(dir)
		if err != nil {
			return nil, fmt.Errorf("read mock OCR folder: %w", err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || filepath.Ext(name) != ".txt" {
				continue
			}
			content, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil {
				return nil, fmt.Errorf("read mock OCR file: %w", err)
			}
			texts[strings.TrimSuffix(name, ".txt")] = string(content)
		}
	}

	fallback, err := os.ReadFile(fallbackPath)
	if err != nil {
		return nil, fmt.Errorf("read mock OCR fallback: %w", err)
	}
	return &MockProvider{texts: texts, fallback: string(fallback), fallbackPath: fallbackPath}, nil
}

// Extract looks up the uploaded name without its extension, so
// receipt-clean.png returns the text of receipt-clean.txt. The name is only
// a lookup key and is never used as a path.
func (m *MockProvider) Extract(ctx context.Context, f fileupload.FileUpload) (string, error) {
	if text, ok := m.texts[nameWithoutExtension(f.FileName)]; ok {
		return text, nil
	}
	slog.Info("mock OCR: no fixture matches the uploaded name, using the fallback",
		"file_name", f.FileName, "fallback", m.fallbackPath)
	return m.fallback, nil
}

// nameWithoutExtension drops any folders and the extension:
// "receipt-clean.png" → "receipt-clean".
func nameWithoutExtension(fileName string) string {
	base := filepath.Base(fileName)
	return strings.TrimSuffix(base, filepath.Ext(base))
}
