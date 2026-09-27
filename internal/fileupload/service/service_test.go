package service

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"

	"auto-itemizer/internal/db"
	"auto-itemizer/internal/fileupload/core"
)

var ctx = context.Background()

// setup returns a Service writing to a temporary storage folder, a temporary
// database, and the storage folder's path.
func setup(t *testing.T) (*Service, *db.DB, string) {
	t.Helper()
	d, err := db.Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })

	storageDir := t.TempDir()
	s, err := New(storageDir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s, d, storageDir
}

// filesIn lists the names of the files in dir.
func filesIn(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	var names []string
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

func TestSaveAndGet(t *testing.T) {
	s, d, storageDir := setup(t)

	f, err := s.Save(ctx, d, "receipt-clean.png", "image/png", strings.NewReader("fake png bytes"))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	if want := filepath.Join(storageDir, "receipts", f.ID+".png"); f.FilePath != want {
		t.Errorf("FilePath = %q, want %q", f.FilePath, want)
	}
	if f.FileName != "receipt-clean.png" || f.ContentType != "image/png" || f.SizeBytes != 14 {
		t.Errorf("saved %+v, want name receipt-clean.png, type image/png, size 14", f)
	}
	content, err := os.ReadFile(f.FilePath)
	if err != nil || string(content) != "fake png bytes" {
		t.Errorf("file on disk = %q (err %v), want the uploaded bytes", content, err)
	}

	got, err := s.Get(ctx, d, f.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != f {
		t.Errorf("Get = %+v, want %+v", got, f)
	}
}

func TestSaveNeverUsesUploadedNameInPath(t *testing.T) {
	s, d, storageDir := setup(t)
	receiptsDir := filepath.Join(storageDir, "receipts")

	f, err := s.Save(ctx, d, "../../evil.png", "image/png", strings.NewReader("x"))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}

	if f.FileName != "../../evil.png" {
		t.Errorf("FileName = %q, want the uploaded name unchanged", f.FileName)
	}
	if f.FilePath != filepath.Join(receiptsDir, f.ID+".png") {
		t.Errorf("FilePath = %q, want <id>.png inside %s", f.FilePath, receiptsDir)
	}
	if _, err := os.Stat(filepath.Join(receiptsDir, "../../evil.png")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a file was written outside the storage folder (stat err %v)", err)
	}
}

func TestExtensionFor(t *testing.T) {
	cases := map[string]string{
		"image/png":       ".png",
		"image/jpeg":      ".jpg",
		"image/gif":       ".gif",
		"image/webp":      ".webp",
		"image/heic":      ".heic",
		"application/pdf": ".pdf",
		"text/plain":      ".txt",
		"image/tiff":      ".bin",
	}
	for contentType, want := range cases {
		if got := extensionFor(contentType); got != want {
			t.Errorf("extensionFor(%q) = %q, want %q", contentType, got, want)
		}
	}
}

func TestSaveRemovesFileWhenInsertFails(t *testing.T) {
	s, d, storageDir := setup(t)
	d.Close() // every query now fails

	if _, err := s.Save(ctx, d, "receipt.png", "image/png", strings.NewReader("x")); err == nil {
		t.Fatal("Save succeeded on a closed database")
	}
	if files := filesIn(t, filepath.Join(storageDir, "receipts")); len(files) != 0 {
		t.Errorf("files left on disk: %v", files)
	}
}

func TestSaveRemovesFileWhenUploadBreaksOff(t *testing.T) {
	s, d, storageDir := setup(t)

	// The body delivers some bytes, then fails, like a client that disconnects.
	body := io.MultiReader(strings.NewReader("partial"), iotest.ErrReader(errors.New("connection reset")))
	if _, err := s.Save(ctx, d, "receipt.png", "image/png", body); err == nil {
		t.Fatal("Save succeeded with a failing body")
	}
	if files := filesIn(t, filepath.Join(storageDir, "receipts")); len(files) != 0 {
		t.Errorf("files left on disk: %v", files)
	}
	var rows int
	if err := d.QueryRowContext(ctx, "SELECT count(*) FROM file_upload").Scan(&rows); err != nil {
		t.Fatalf("count file_upload: %v", err)
	}
	if rows != 0 {
		t.Errorf("file_upload rows = %d, want 0", rows)
	}
}

// This is the cleanup path of POST /receipts (ARCHITECTURE.md §3.1): Save
// succeeds, a later step in the same transaction fails, the transaction
// rolls back and the caller deletes the file.
func TestSaveInRolledBackTransactionThenDelete(t *testing.T) {
	s, d, storageDir := setup(t)

	var saved core.FileUpload
	receiptInsertFailed := errors.New("receipt insert failed")
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		f, err := s.Save(ctx, tx, "receipt.pdf", "application/pdf", strings.NewReader("%PDF"))
		if err != nil {
			return err
		}
		saved = f
		return receiptInsertFailed
	})
	if !errors.Is(err, receiptInsertFailed) {
		t.Fatalf("InTx error = %v, want the simulated failure", err)
	}

	if err := s.Delete(saved); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if files := filesIn(t, filepath.Join(storageDir, "receipts")); len(files) != 0 {
		t.Errorf("files left on disk: %v", files)
	}
	if _, err := s.Get(ctx, d, saved.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Get after rollback: err = %v, want sql.ErrNoRows", err)
	}
}

func TestDeleteMissingFileIsNotAnError(t *testing.T) {
	s, d, _ := setup(t)

	f, err := s.Save(ctx, d, "receipt.txt", "text/plain", strings.NewReader("TOTAL 1.00"))
	if err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := s.Delete(f); err != nil {
		t.Fatalf("first Delete: %v", err)
	}
	if err := s.Delete(f); err != nil {
		t.Errorf("second Delete of the same file: %v", err)
	}
}

func TestGetUnknownID(t *testing.T) {
	s, d, _ := setup(t)
	if _, err := s.Get(ctx, d, "no-such-id"); !errors.Is(err, sql.ErrNoRows) {
		t.Errorf("Get unknown id: err = %v, want sql.ErrNoRows", err)
	}
}
