// Package db opens the SQLite database, applies the migrations and runs
// transactions that services pass to each other (ARCHITECTURE.md §1.1).
package db

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver
)

// Connection settings, applied to every connection in the pool:
//   - foreign_keys: enforce the REFERENCES clauses.
//   - journal_mode WAL: reads don't wait for a write to finish.
//   - busy_timeout: a second writer waits up to 5 s instead of failing at once.
//   - _txlock=immediate: every transaction takes the write lock at BEGIN, so
//     two concurrent writes queue instead of both failing when they try to
//     upgrade from a read lock. A lost update is still caught by the
//     updated_at check (ErrConflict).
const dsnParams = "?_pragma=foreign_keys(1)" +
	"&_pragma=journal_mode(WAL)" +
	"&_pragma=busy_timeout(5000)" +
	"&_txlock=immediate"

// ErrConflict means a conditional write matched no row: another request
// changed the row after it was read (ARCHITECTURE.md §3.0).
var ErrConflict = errors.New("row was changed by another request")

// Values for created_by and updated_by. There is no auth (ARCHITECTURE.md §2).
const (
	ActorSystem = "system" // writes made by the pipeline
	ActorUser   = "user"   // writes made through PATCH
)

// Querier is implemented by both *sql.DB and *sql.Tx, so a repository method
// works on its own or inside a transaction opened by another service.
type Querier interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// DB is the connection pool.
type DB struct {
	*sql.DB
}

// Open connects to the SQLite file at path, creating the file and its folder
// if needed, and applies the migrations the database hasn't had yet (migrate.go).
func Open(ctx context.Context, path string) (*DB, error) {
	// An empty path makes the driver read dsnParams as the file name and skip
	// every setting, and ":memory:" gives each pooled connection its own empty
	// database. Neither fails on its own, so refuse both here.
	if path == "" || path == ":memory:" {
		return nil, fmt.Errorf("database path %q: need a file path", path)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create database folder: %w", err)
	}
	sqlDB, err := sql.Open("sqlite", path+dsnParams)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	d := &DB{sqlDB}
	if err := migrate(ctx, d, migrationFiles); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return d, nil
}

// InTx runs fn in one transaction. It commits if fn returns nil, and rolls
// back if fn returns an error or panics.
func (d *DB) InTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := d.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() {
		if p := recover(); p != nil {
			tx.Rollback()
			panic(p)
		}
	}()
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}
	return nil
}

// InReadTx runs fn in a read-only transaction, so all its reads see the same
// snapshot of the database. Unlike InTx it doesn't take the write lock: the
// driver ignores _txlock=immediate for read-only transactions, so writers
// don't wait for readers.
func (d *DB) InReadTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := d.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin read transaction: %w", err)
	}
	defer tx.Rollback() // nothing to commit; this ends the snapshot
	return fn(tx)
}

// ExpectOneRow checks the result of a conditional write such as
// UPDATE … WHERE id = ? AND updated_at = ?. It returns ErrConflict when no
// row matched.
func ExpectOneRow(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	switch n {
	case 0:
		return ErrConflict
	case 1:
		return nil
	default:
		return fmt.Errorf("conditional write changed %d rows, want 1", n)
	}
}

// timeLayout is fixed width, so stored timestamps also sort correctly as text.
const timeLayout = "2006-01-02T15:04:05.000000Z"

// Now returns the current UTC time in the stored timestamp format.
func Now() string {
	return time.Now().UTC().Format(timeLayout)
}

// NextUpdatedAt returns the updated_at to write over prev: the current time,
// or 1µs after prev if the clock hasn't moved past it. Every write therefore
// changes updated_at, which the conflict check relies on.
func NextUpdatedAt(prev string) string {
	now := time.Now().UTC().Truncate(time.Microsecond)
	if p, err := time.Parse(timeLayout, prev); err == nil && !now.After(p) {
		now = p.Add(time.Microsecond)
	}
	return now.Format(timeLayout)
}
