// Package service is the FileUploadService (ARCHITECTURE.md §1): it writes
// uploaded files to disk and records them in the file_upload table.
package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"

	"auto-itemizer/internal/db"
	"auto-itemizer/internal/fileupload/core"
	"auto-itemizer/internal/fileupload/repository"

	"github.com/google/uuid"
)

// Service saves, reads and deletes uploaded files.
type Service struct {
	dir string // folder the files are written to: <storageDir>/receipts
}

// New returns a Service that writes files to <storageDir>/receipts, creating
// that folder if needed.
func New(storageDir string) (*Service, error) {
	dir := filepath.Join(storageDir, "receipts")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create storage folder: %w", err)
	}
	return &Service{dir: dir}, nil
}

// Save writes body to <dir>/<id>.<ext> and inserts the file_upload row using
// q, which is normally the caller's transaction. If writing or inserting
// fails, Save removes the file before returning the error. If the caller's
// transaction fails after Save has returned, the caller must call Delete.
func (s *Service) Save(ctx context.Context, q db.Querier, fileName, contentType string, body io.Reader) (core.FileUpload, error) {
	id := uuid.NewString()
	path := filepath.Join(s.dir, id+extensionFor(contentType))

	size, err := writeFile(path, body)
	if err != nil {
		return core.FileUpload{}, err
	}

	now := db.Now()
	f := core.FileUpload{
		ID:          id,
		FilePath:    path,
		FileName:    fileName,
		ContentType: contentType,
		SizeBytes:   size,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
	if err := repository.InsertFileUpload(ctx, q, f); err != nil {
		os.Remove(path)
		return core.FileUpload{}, err
	}
	return f, nil
}

// Get returns the file_upload row with the given id.
func (s *Service) Get(ctx context.Context, q db.Querier, id string) (core.FileUpload, error) {
	f, err := repository.SelectFileUpload(ctx, q, id)
	if err != nil {
		return core.FileUpload{}, fmt.Errorf("get file_upload %s: %w", id, err)
	}
	return f, nil
}

// Delete removes the file from disk. It is the cleanup step when the upload
// transaction fails (ARCHITECTURE.md §3.1), so a file that is already gone is
// not an error.
func (s *Service) Delete(f core.FileUpload) error {
	err := os.Remove(f.FilePath)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	return err
}

// extensions maps the allowed content types to the extension of the stored
// file. The extension never comes from the uploaded name.
var extensions = map[string]string{
	"image/png":       ".png",
	"image/jpeg":      ".jpg",
	"image/gif":       ".gif",
	"image/webp":      ".webp",
	"image/heic":      ".heic",
	"application/pdf": ".pdf",
	"text/plain":      ".txt",
}

// extensionFor returns the stored file's extension, or .bin for any other
// type (such as a less common image/* type).
func extensionFor(contentType string) string {
	if ext, ok := extensions[contentType]; ok {
		return ext
	}
	return ".bin"
}

// writeFile streams body into a new file at path and returns the number of
// bytes written. If anything fails, it removes the partial file.
func writeFile(path string, body io.Reader) (int64, error) {
	// O_EXCL: fail rather than overwrite if the file somehow exists already.
	out, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return 0, fmt.Errorf("create file: %w", err)
	}
	size, copyErr := io.Copy(out, body)
	closeErr := out.Close()
	if copyErr != nil || closeErr != nil {
		os.Remove(path)
		return 0, fmt.Errorf("write file: %w", errors.Join(copyErr, closeErr))
	}
	return size, nil
}
