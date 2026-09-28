// Command server runs the receipt API: go run ./cmd/server from the repo
// root. It reads the settings (internal/config), wires the services, listens
// on 127.0.0.1 and stops cleanly on Ctrl-C or SIGTERM.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"auto-itemizer/internal/config"
	"auto-itemizer/internal/db"
	expenseservice "auto-itemizer/internal/expense/service"
	fileuploadservice "auto-itemizer/internal/fileupload/service"
	"auto-itemizer/internal/httpapi"
	"auto-itemizer/internal/ocr"
	"auto-itemizer/internal/receipt/core/parser"
	receiptservice "auto-itemizer/internal/receipt/service"
)

const (
	ocrTimeout      = 30 * time.Second // the longest one OCR call may take
	shutdownTimeout = 10 * time.Second // how long requests in flight get to finish after Ctrl-C or SIGTERM
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("bad setting", "error", err)
		os.Exit(1)
	}
	// ctx ends at the first Ctrl-C (SIGINT) or SIGTERM, which starts the
	// shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	err = run(ctx, cfg)
	stop()
	if err != nil {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}

// run starts the service and blocks until ctx ends, then shuts it down. The
// OCR provider comes first, so a wrong FIXTURES_DIR fails before any file or
// folder is created.
func run(ctx context.Context, cfg config.Config) error {
	provider, err := ocrProvider(cfg)
	if err != nil {
		return err
	}
	d, err := db.Open(ctx, cfg.DBPath)
	if err != nil {
		return err
	}
	defer d.Close()

	handler, err := newHandler(ctx, cfg, d, provider)
	if err != nil {
		return err
	}
	// 127.0.0.1: only this machine can connect, because the API has no login.
	ln, err := net.Listen("tcp", "127.0.0.1:"+cfg.Port)
	if err != nil {
		return err
	}
	slog.Info("listening", "url", "http://localhost:"+cfg.Port, "mock_ocr", cfg.MockOCR,
		"db", cfg.DBPath, "storage", cfg.StorageDir)
	return serve(ctx, ln, handler)
}

// ocrProvider returns the OCR provider that MOCK_OCR selects (ARCHITECTURE.md
// §1.2). The mock provider reads every fixture now, so a missing folder fails
// at startup rather than on the first request.
func ocrProvider(cfg config.Config) (ocr.Provider, error) {
	if !cfg.MockOCR {
		return ocr.LiveProvider{}, nil
	}
	dirs := []string{filepath.Join(cfg.FixturesDir, "task-a"), filepath.Join(cfg.FixturesDir, "mock-ocr")}
	mock, err := ocr.NewMockProvider(dirs, cfg.MockOCRFallback)
	if err != nil {
		return nil, fmt.Errorf("%w (run from the repo root, or set FIXTURES_DIR)", err)
	}
	return mock, nil
}

// newHandler builds the services in the order they need each other and
// returns the API (IMPLEMENTATION_PLAN.md §6).
func newHandler(ctx context.Context, cfg config.Config, d *db.DB, provider ocr.Provider) (http.Handler, error) {
	files, err := fileuploadservice.New(cfg.StorageDir)
	if err != nil {
		return nil, err
	}
	expenses := expenseservice.New(d)
	names, err := expenses.TaxNames(ctx)
	if err != nil {
		return nil, err
	}
	// Without tax names, a line like "VAT 19% 2.85" would be read as an item,
	// and the receipt could still come out COMPLETE: a silent wrong answer.
	if len(names) == 0 {
		return nil, errors.New("tax_master has no tax names, so no line would be read as a tax; check the seed in internal/db/schema.sql")
	}
	receipts := receiptservice.New(d, files, ocr.New(provider, ocrTimeout), expenses, parser.New(names))
	expenses.SetParsedReceiptSource(receipts) // the two services need each other
	return httpapi.NewHandler(receipts, expenses), nil
}

// serve answers requests on ln until ctx ends, then shuts down gracefully:
// it stops accepting connections and gives the requests in flight up to
// shutdownTimeout to finish.
func serve(ctx context.Context, ln net.Listener, h http.Handler) error {
	srv := &http.Server{
		Handler:           h,
		ReadHeaderTimeout: 5 * time.Second, // a slow client can't hold a connection open forever
		ReadTimeout:       time.Minute,     // the whole request, including an 11 MB upload
		WriteTimeout:      2 * time.Minute, // above ocrTimeout: when it fires, the handler keeps running
		IdleTimeout:       2 * time.Minute,
	}
	served := make(chan error, 1)
	go func() { served <- srv.Serve(ln) }()

	select {
	case err := <-served:
		return err // Serve failed; it never returns nil
	case <-ctx.Done():
	}
	slog.Info("shutting down")
	// A fresh context: ctx is already cancelled and would stop the drain at
	// once. Serve now returns http.ErrServerClosed, which is the normal end.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), shutdownTimeout)
	defer cancel()
	return srv.Shutdown(shutdownCtx)
}
