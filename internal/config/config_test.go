package config

import (
	"path/filepath"
	"strings"
	"testing"
)

// clearEnv makes every setting empty for the test, which counts as unset.
func clearEnv(t *testing.T) {
	t.Helper()
	for _, name := range []string{"PORT", "STORAGE_DIR", "DB_PATH", "FIXTURES_DIR", "MOCK_OCR", "MOCK_OCR_FALLBACK"} {
		t.Setenv(name, "")
	}
}

func load(t *testing.T) Config {
	t.Helper()
	cfg, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	return cfg
}

func TestLoadDefaults(t *testing.T) {
	clearEnv(t)
	want := Config{
		Port:            "8080",
		StorageDir:      "storage",
		DBPath:          filepath.Join("storage", "auto-itemizer.db"),
		FixturesDir:     "fixtures",
		MockOCR:         true,
		MockOCRFallback: filepath.Join("fixtures", "task-a", "receipt-clean.txt"),
	}
	if got := load(t); got != want {
		t.Errorf("Load() = %+v\nwant %+v", got, want)
	}
}

// The database and the fallback text follow their folders unless they are
// set themselves.
func TestLoadFollowsFolders(t *testing.T) {
	clearEnv(t)
	t.Setenv("STORAGE_DIR", "/data")
	t.Setenv("FIXTURES_DIR", "/fx")
	got := load(t)
	if got.DBPath != filepath.Join("/data", "auto-itemizer.db") {
		t.Errorf("DBPath = %s, want it inside STORAGE_DIR", got.DBPath)
	}
	if got.MockOCRFallback != filepath.Join("/fx", "task-a", "receipt-clean.txt") {
		t.Errorf("MockOCRFallback = %s, want it inside FIXTURES_DIR", got.MockOCRFallback)
	}
}

func TestLoadFromEnv(t *testing.T) {
	clearEnv(t)
	t.Setenv("PORT", "9090")
	t.Setenv("STORAGE_DIR", "/s")
	t.Setenv("DB_PATH", "/d/x.db")
	t.Setenv("FIXTURES_DIR", "/f")
	t.Setenv("MOCK_OCR", "false")
	t.Setenv("MOCK_OCR_FALLBACK", "/f/other.txt")
	want := Config{Port: "9090", StorageDir: "/s", DBPath: "/d/x.db", FixturesDir: "/f", MockOCR: false, MockOCRFallback: "/f/other.txt"}
	if got := load(t); got != want {
		t.Errorf("Load() = %+v\nwant %+v", got, want)
	}
}

func TestLoadRejectsBadValues(t *testing.T) {
	for _, tc := range []struct{ name, value string }{
		{"PORT", "abc"},
		{"PORT", "0"},
		{"PORT", "70000"},
		{"MOCK_OCR", "maybe"},
	} {
		t.Run(tc.name+"="+tc.value, func(t *testing.T) {
			clearEnv(t)
			t.Setenv(tc.name, tc.value)
			if _, err := Load(); err == nil || !strings.Contains(err.Error(), tc.name+"="+tc.value) {
				t.Errorf("Load() error = %v, want one naming %s=%s", err, tc.name, tc.value)
			}
		})
	}
}
