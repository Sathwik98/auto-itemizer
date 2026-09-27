package ocr

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"auto-itemizer/internal/fileupload"
)

var ctx = context.Background()

// writeFiles creates a temporary folder holding the given files.
func writeFiles(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// newTestMock returns a MockProvider over two fixture folders, like
// fixtures/task-a and fixtures/mock-ocr, and a separate fallback file.
func newTestMock(t *testing.T) *MockProvider {
	t.Helper()
	taskA := writeFiles(t, map[string]string{
		"receipt-clean.txt":    "CLEAN",
		"receipt-mismatch.txt": "MISMATCH",
		"gold.json":            "{}",
	})
	mockOCR := writeFiles(t, map[string]string{"not-a-receipt.txt": "LETTER"})
	fallback := filepath.Join(writeFiles(t, map[string]string{"fallback.txt": "FALLBACK"}), "fallback.txt")

	m, err := NewMockProvider([]string{taskA, mockOCR}, fallback)
	if err != nil {
		t.Fatalf("NewMockProvider: %v", err)
	}
	return m
}

// upload returns a file_upload whose stored file doesn't exist, which proves
// the mock never reads it.
func upload(fileName string) fileupload.FileUpload {
	return fileupload.FileUpload{ID: "file-1", FileName: fileName, FilePath: "/no/such/file"}
}

func TestMockReturnsFixtureByUploadedName(t *testing.T) {
	m := newTestMock(t)
	cases := map[string]string{
		"receipt-clean.png":    "CLEAN",
		"receipt-clean.txt":    "CLEAN",
		"receipt-mismatch.jpg": "MISMATCH",
		"not-a-receipt.pdf":    "LETTER", // from the second folder
	}
	for fileName, want := range cases {
		got, err := m.Extract(ctx, upload(fileName))
		if err != nil || got != want {
			t.Errorf("Extract(%q) = %q, %v; want %q", fileName, got, err, want)
		}
	}
}

func TestMockFallsBackAndLogsIt(t *testing.T) {
	m := newTestMock(t)

	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })

	for _, fileName := range []string{
		"unknown.png",
		"gold.png",             // gold.json isn't a .txt fixture
		"../../etc/passwd.png", // only "passwd" is looked up, and it isn't a fixture
		"",
	} {
		got, err := m.Extract(ctx, upload(fileName))
		if err != nil || got != "FALLBACK" {
			t.Errorf("Extract(%q) = %q, %v; want the fallback", fileName, got, err)
		}
	}
	if !strings.Contains(logs.String(), "file_name=unknown.png") {
		t.Errorf("fallback was not logged; logs:\n%s", logs.String())
	}
}

func TestNewMockProviderFailsOnMissingFiles(t *testing.T) {
	dir := writeFiles(t, map[string]string{"receipt-clean.txt": "CLEAN"})
	fallback := filepath.Join(dir, "receipt-clean.txt")

	if _, err := NewMockProvider([]string{dir, filepath.Join(dir, "missing")}, fallback); err == nil {
		t.Error("missing fixture folder: want an error")
	}
	if _, err := NewMockProvider([]string{dir}, filepath.Join(dir, "missing.txt")); err == nil {
		t.Error("missing fallback file: want an error")
	}
}

// fakeProvider is a Provider for tests. It returns text and err, or, with
// hang set, waits until the context is cancelled.
type fakeProvider struct {
	text string
	err  error
	hang bool
}

func (p fakeProvider) Extract(ctx context.Context, f fileupload.FileUpload) (string, error) {
	if p.hang {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return p.text, p.err
}

func TestServiceReturnsProviderText(t *testing.T) {
	s := New(newTestMock(t), time.Second)
	got, err := s.Extract(ctx, upload("receipt-clean.png"))
	if err != nil || got != "CLEAN" {
		t.Errorf("Extract = %q, %v; want CLEAN", got, err)
	}
}

func TestServicePassesProviderErrors(t *testing.T) {
	vendorDown := errors.New("vendor down")
	cases := []struct {
		name     string
		provider Provider
		want     error
	}{
		{"provider error", fakeProvider{err: vendorDown}, vendorDown},
		// ReceiptService checks for ErrNotConfigured to return 501.
		{"live OCR", LiveProvider{}, ErrNotConfigured},
	}
	for _, tc := range cases {
		_, err := New(tc.provider, time.Second).Extract(ctx, upload("receipt.png"))
		if !errors.Is(err, tc.want) {
			t.Errorf("%s: err = %v, want %v", tc.name, err, tc.want)
		}
	}
}

func TestServiceTimesOut(t *testing.T) {
	s := New(fakeProvider{hang: true}, 20*time.Millisecond)

	start := time.Now()
	_, err := s.Extract(ctx, upload("receipt.png"))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("took %v, want the call cut off after about 20ms", elapsed)
	}
}
