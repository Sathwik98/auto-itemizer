// Package config reads the service's settings from environment variables
// (IMPLEMENTATION_PLAN.md §2). Every setting has a default, so the service
// runs with no configuration: go run ./cmd/server from the repo root.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
)

// Config holds the service's settings.
type Config struct {
	Port            string // PORT: the port to listen on, on 127.0.0.1 only
	StorageDir      string // STORAGE_DIR: uploaded files go to <StorageDir>/receipts
	DBPath          string // DB_PATH: the SQLite file
	FixturesDir     string // FIXTURES_DIR: the mock OCR reads <FixturesDir>/task-a and <FixturesDir>/mock-ocr
	MockOCR         bool   // MOCK_OCR: false selects the live provider, so process returns 501
	MockOCRFallback string // MOCK_OCR_FALLBACK: the text used when no fixture matches the upload's name
}

// Load reads the settings from the environment. An unset or empty variable
// gets its default. A value that can't be used is an error naming the
// variable, never a silent default: MOCK_OCR=flase must not quietly run in
// mock mode.
func Load() (Config, error) {
	cfg := Config{
		Port:        env("PORT", "8080"),
		StorageDir:  env("STORAGE_DIR", "storage"),
		FixturesDir: env("FIXTURES_DIR", "fixtures"),
	}
	// These two follow the folders above unless they are set themselves.
	cfg.DBPath = env("DB_PATH", filepath.Join(cfg.StorageDir, "auto-itemizer.db"))
	cfg.MockOCRFallback = env("MOCK_OCR_FALLBACK", filepath.Join(cfg.FixturesDir, "task-a", "receipt-clean.txt"))

	if port, err := strconv.Atoi(cfg.Port); err != nil || port < 1 || port > 65535 {
		return Config{}, fmt.Errorf("PORT=%s: want a number from 1 to 65535", cfg.Port)
	}
	mockOCR := env("MOCK_OCR", "true")
	var err error
	if cfg.MockOCR, err = strconv.ParseBool(mockOCR); err != nil {
		return Config{}, fmt.Errorf("MOCK_OCR=%s: want true or false", mockOCR)
	}
	return cfg, nil
}

// env returns the environment variable name, or def when it is unset or
// empty.
func env(name, def string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return def
}
