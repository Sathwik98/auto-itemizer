package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
)

var ctx = context.Background()

func openTestDB(t *testing.T) *DB {
	t.Helper()
	d, err := Open(ctx, filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { d.Close() })
	return d
}

func mustExec(t *testing.T, q Querier, query string, args ...any) sql.Result {
	t.Helper()
	res, err := q.ExecContext(ctx, query, args...)
	if err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
	return res
}

// The insert helpers fill every NOT NULL column, so each test only passes
// the values it checks.

func insertFile(q Querier, id string) error {
	now := Now()
	_, err := q.ExecContext(ctx, `
		INSERT INTO file_upload (id, file_path, file_name, content_type, size_bytes,
		                         created_at, created_by, updated_at, updated_by)
		VALUES (?, 'storage/receipts/x.txt', 'x.txt', 'text/plain', 1, ?, 'system', ?, 'system')`,
		id, now, now)
	return err
}

func insertReceipt(q Querier, id, fileID, status, updatedAt string) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO receipts (id, file_id, status, created_at, created_by, updated_at, updated_by)
		VALUES (?, ?, ?, ?, 'system', ?, 'system')`,
		id, fileID, status, updatedAt, updatedAt)
	return err
}

func insertOCR(q Querier, id, receiptID string) error {
	now := Now()
	_, err := q.ExecContext(ctx, `
		INSERT INTO receipt_ocr (id, receipt_id, ocr_payload, created_at, created_by, updated_at, updated_by)
		VALUES (?, ?, 'TOTAL 1.00', ?, 'system', ?, 'system')`,
		id, receiptID, now, now)
	return err
}

func insertExpense(q Querier, id, receiptID string) error {
	now := Now()
	_, err := q.ExecContext(ctx, `
		INSERT INTO expense (id, receipt_id, itemization_status, merchant, date, currency, total,
		                     created_at, created_by, updated_at, updated_by)
		VALUES (?, ?, 'COMPLETE', 'Cafe Mitte', '2026-03-12', 'EUR', '17.85', ?, 'system', ?, 'system')`,
		id, receiptID, now, now)
	return err
}

// wantConstraintError fails the test unless err is a constraint violation,
// so a typo in a test query can't pass as a rejected row.
func wantConstraintError(t *testing.T, err error, what string) {
	t.Helper()
	if err == nil {
		t.Errorf("%s: insert was accepted, want a constraint error", what)
	} else if !strings.Contains(err.Error(), "constraint failed") {
		t.Errorf("%s: got %v, want a constraint error", what, err)
	}
}

func countRows(t *testing.T, q Querier, table string) int {
	t.Helper()
	var n int
	if err := q.QueryRowContext(ctx, "SELECT count(*) FROM "+table).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", table, err)
	}
	return n
}

func TestOpenAppliesSchemaAndSeedOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested", "folder", "test.db")
	first, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("first Open: %v", err)
	}
	first.Close()

	d, err := Open(ctx, path)
	if err != nil {
		t.Fatalf("second Open: %v", err)
	}
	defer d.Close()

	rows, err := d.QueryContext(ctx, "SELECT name FROM tax_master ORDER BY name")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		names = append(names, name)
	}
	// Opened twice, yet each seed name appears once.
	want := []string{"CESS", "CGST", "GST", "HST", "IGST", "IVA", "MWST", "PST",
		"QST", "SALES TAX", "SGST", "TAX", "TVA", "UST", "UTGST", "VAT"}
	if !slices.Equal(names, want) {
		t.Errorf("tax_master names = %v, want %v", names, want)
	}

	pragmas := map[string]string{"foreign_keys": "1", "journal_mode": "wal", "busy_timeout": "5000"}
	for pragma, want := range pragmas {
		var got string
		if err := d.QueryRowContext(ctx, "PRAGMA "+pragma).Scan(&got); err != nil {
			t.Fatalf("PRAGMA %s: %v", pragma, err)
		}
		if got != want {
			t.Errorf("PRAGMA %s = %q, want %q", pragma, got, want)
		}
	}
}

func TestOpenRejectsPathsWithoutAFile(t *testing.T) {
	// Run in an empty folder, so that if the check ever breaks, the stray
	// database file lands there and not in the repository.
	t.Chdir(t.TempDir())
	for _, path := range []string{"", ":memory:"} {
		d, err := Open(ctx, path)
		if err == nil {
			d.Close()
			t.Errorf("Open(%q) succeeded, want an error", path)
		}
	}
}

func TestOneActiveRowPerReceipt(t *testing.T) {
	tables := []struct {
		table  string
		insert func(q Querier, id, receiptID string) error
	}{
		{"receipt_ocr", insertOCR},
		{"expense", insertExpense},
	}
	for _, tc := range tables {
		t.Run(tc.table, func(t *testing.T) {
			d := openTestDB(t)
			if err := insertFile(d, "file-1"); err != nil {
				t.Fatal(err)
			}
			if err := insertReceipt(d, "receipt-1", "file-1", "PROCESSED", Now()); err != nil {
				t.Fatal(err)
			}

			if err := tc.insert(d, "row-a", "receipt-1"); err != nil {
				t.Fatalf("first active row: %v", err)
			}
			wantConstraintError(t, tc.insert(d, "row-b", "receipt-1"), "second active row")

			mustExec(t, d, "UPDATE "+tc.table+" SET is_deleted = 1 WHERE id = 'row-a'")
			if err := tc.insert(d, "row-b", "receipt-1"); err != nil {
				t.Fatalf("active row next to a soft-deleted one: %v", err)
			}
			wantConstraintError(t, tc.insert(d, "row-c", "receipt-1"), "second active row after a soft delete")
		})
	}
}

func TestConstraints(t *testing.T) {
	d := openTestDB(t)
	for _, id := range []string{"file-1", "file-2"} {
		if err := insertFile(d, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := insertReceipt(d, "receipt-1", "file-1", "UPLOADED", Now()); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		insert func() error
	}{
		{"unknown file_id", func() error {
			return insertReceipt(d, "receipt-2", "missing-file", "UPLOADED", Now())
		}},
		{"second receipt for the same file", func() error {
			return insertReceipt(d, "receipt-3", "file-1", "UPLOADED", Now())
		}},
		{"unknown receipt status", func() error {
			return insertReceipt(d, "receipt-4", "file-2", "DONE", Now())
		}},
		{"unknown receipt_id on expense", func() error {
			return insertExpense(d, "expense-1", "missing-receipt")
		}},
		{"duplicate tax name", func() error {
			_, err := d.ExecContext(ctx, `
				INSERT INTO tax_master (id, name, created_at, created_by, updated_at, updated_by)
				VALUES ('tax-x', 'VAT', ?, 'system', ?, 'system')`, Now(), Now())
			return err
		}},
	}
	for _, tc := range cases {
		wantConstraintError(t, tc.insert(), tc.name)
	}
}

func TestInTx(t *testing.T) {
	d := openTestDB(t)

	if err := d.InTx(ctx, func(tx *sql.Tx) error {
		return insertFile(tx, "committed")
	}); err != nil {
		t.Fatalf("InTx commit: %v", err)
	}

	boom := errors.New("boom")
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		if err := insertFile(tx, "rolled-back-on-error"); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("InTx error = %v, want boom", err)
	}

	func() {
		defer func() {
			if p := recover(); p != "panic in tx" {
				t.Errorf("recovered %v, want the original panic", p)
			}
		}()
		d.InTx(ctx, func(tx *sql.Tx) error {
			if err := insertFile(tx, "rolled-back-on-panic"); err != nil {
				return err
			}
			panic("panic in tx")
		})
	}()

	var id string
	if err := d.QueryRowContext(ctx, "SELECT id FROM file_upload").Scan(&id); err != nil {
		t.Fatal(err)
	}
	if n := countRows(t, d, "file_upload"); n != 1 || id != "committed" {
		t.Errorf("file_upload has %d rows (first %q), want only \"committed\"", n, id)
	}
}

// TestInReadTx checks that a read transaction keeps one snapshot and doesn't
// make writers wait. With the write lock (InTx) the writer below would wait
// for busy_timeout and the test would time out.
func TestInReadTx(t *testing.T) {
	d := openTestDB(t)
	if err := insertFile(d, "before"); err != nil {
		t.Fatal(err)
	}

	err := d.InReadTx(ctx, func(tx *sql.Tx) error {
		before := countRows(t, tx, "file_upload")

		written := make(chan error, 1)
		go func() {
			written <- d.InTx(ctx, func(w *sql.Tx) error { return insertFile(w, "during") })
		}()
		select {
		case err := <-written:
			if err != nil {
				t.Fatalf("write during a read transaction: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the writer waited for the read transaction")
		}

		if after := countRows(t, tx, "file_upload"); after != before {
			t.Errorf("read transaction saw %d rows, then %d; want the same snapshot", before, after)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("InReadTx: %v", err)
	}
	if n := countRows(t, d, "file_upload"); n != 2 {
		t.Errorf("after the read transaction: %d rows, want 2", n)
	}
}

func TestConditionalWrite(t *testing.T) {
	d := openTestDB(t)
	read := Now()
	if err := insertFile(d, "file-1"); err != nil {
		t.Fatal(err)
	}
	if err := insertReceipt(d, "receipt-1", "file-1", "UPLOADED", read); err != nil {
		t.Fatal(err)
	}

	update := func(status string) error {
		res := mustExec(t, d, `
			UPDATE receipts SET status = ?, updated_at = ?
			WHERE id = 'receipt-1' AND updated_at = ?`,
			status, NextUpdatedAt(read), read)
		return ExpectOneRow(res)
	}

	if err := update("OCR_EXTRACTED"); err != nil {
		t.Fatalf("first write with the value read: %v", err)
	}
	// A second request that read the same updated_at must now lose.
	if err := update("FAILED"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale write error = %v, want ErrConflict", err)
	}
}

func TestExpectOneRowRejectsManyRows(t *testing.T) {
	d := openTestDB(t)
	for _, id := range []string{"file-1", "file-2"} {
		if err := insertFile(d, id); err != nil {
			t.Fatal(err)
		}
	}

	// A conditional write that forgot "WHERE id = ?" changes every row.
	res := mustExec(t, d, "UPDATE file_upload SET updated_by = 'user'")
	if err := ExpectOneRow(res); err == nil || errors.Is(err, ErrConflict) {
		t.Errorf("ExpectOneRow after 2 changed rows = %v, want an error that is not ErrConflict", err)
	}
}

func TestConcurrentWritesQueue(t *testing.T) {
	d := openTestDB(t)
	const writers = 10

	// Each transaction reads, then writes. Without _txlock=immediate the
	// writers would hold read locks at the same time and fail with
	// SQLITE_BUSY when upgrading; with it they wait their turn.
	var wg sync.WaitGroup
	errs := make(chan error, writers)
	for i := range writers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs <- d.InTx(ctx, func(tx *sql.Tx) error {
				var n int
				if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM file_upload").Scan(&n); err != nil {
					return err
				}
				time.Sleep(5 * time.Millisecond)
				return insertFile(tx, fmt.Sprintf("file-%d", i))
			})
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("concurrent InTx: %v", err)
		}
	}
	if n := countRows(t, d, "file_upload"); n != writers {
		t.Errorf("file_upload rows = %d, want %d", n, writers)
	}
}

func TestNextUpdatedAt(t *testing.T) {
	parse := func(s string) time.Time {
		t.Helper()
		v, err := time.Parse(timeLayout, s)
		if err != nil {
			t.Fatalf("parse %q: %v", s, err)
		}
		return v
	}

	if got := Now(); len(got) != len(timeLayout) {
		t.Errorf("Now() = %q, want fixed width %d", got, len(timeLayout))
	}

	future := time.Now().UTC().Add(time.Hour).Truncate(time.Microsecond)
	got := parse(NextUpdatedAt(future.Format(timeLayout)))
	if want := future.Add(time.Microsecond); !got.Equal(want) {
		t.Errorf("NextUpdatedAt(future) = %v, want %v", got, want)
	}

	prev := Now()
	for range 1000 {
		next := NextUpdatedAt(prev)
		if next <= prev {
			t.Fatalf("NextUpdatedAt(%q) = %q, not later", prev, next)
		}
		prev = next
	}
}
