package db

import (
	"cmp"
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"log/slog"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// migrationFiles holds the numbered migrations, built into the binary.
//
//go:embed migrations/*.sql
var migrationFiles embed.FS

// migrationName is how a migration file must be named: a unique 4-digit
// number, then a short description, e.g. 0001_create_tables.sql.
var migrationName = regexp.MustCompile(`^(\d{4})_[a-z0-9_]+\.sql$`)

// migration is one numbered SQL file.
type migration struct {
	version int
	name    string // the file name without .sql, e.g. "0001_create_tables"
	sql     string
}

// migrate applies the migrations in files that the database hasn't had yet,
// in number order (IMPLEMENTATION_PLAN.md §3). Each one runs in its own
// transaction together with its schema_migrations row, so a failing file
// leaves nothing half-applied.
func migrate(ctx context.Context, d *DB, files fs.FS) error {
	all, err := readMigrations(files)
	if err != nil {
		return err
	}
	if _, err := d.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
		    version    INTEGER PRIMARY KEY,
		    name       TEXT NOT NULL,
		    applied_at TEXT NOT NULL
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}
	for _, m := range all {
		applied, err := d.applyMigration(ctx, m)
		if err != nil {
			return fmt.Errorf("migration %s: %w", m.name, err)
		}
		if applied {
			slog.Info("applied migration", "version", m.version, "name", m.name)
		}
	}
	return nil
}

// applyMigration runs m unless schema_migrations already lists it. The check
// is inside the transaction, which holds SQLite's write lock, so two servers
// starting at once never run a migration twice.
func (d *DB) applyMigration(ctx context.Context, m migration) (bool, error) {
	applied := false
	err := d.InTx(ctx, func(tx *sql.Tx) error {
		var n int
		if err := tx.QueryRowContext(ctx, `SELECT count(*) FROM schema_migrations WHERE version = ?`, m.version).Scan(&n); err != nil {
			return err
		}
		if n > 0 {
			return nil // already applied
		}
		if _, err := tx.ExecContext(ctx, m.sql); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)`,
			m.version, m.name, Now()); err != nil {
			return err
		}
		applied = true
		return nil
	})
	return applied, err
}

// readMigrations lists the migration files in number order. A badly named
// file, or two files with the same number, is an error rather than a guess
// at the order.
func readMigrations(files fs.FS) ([]migration, error) {
	paths, err := fs.Glob(files, "migrations/*")
	if err != nil {
		return nil, err
	}
	var all []migration
	seen := make(map[int]string)
	for _, p := range paths {
		file := path.Base(p)
		match := migrationName.FindStringSubmatch(file)
		if match == nil {
			return nil, fmt.Errorf("migration file %s: want a name like 0001_create_tables.sql", file)
		}
		version, _ := strconv.Atoi(match[1])
		if other, ok := seen[version]; ok {
			return nil, fmt.Errorf("migration files %s and %s have the same number", other, file)
		}
		seen[version] = file
		content, err := fs.ReadFile(files, p)
		if err != nil {
			return nil, fmt.Errorf("read migration %s: %w", file, err)
		}
		all = append(all, migration{version: version, name: strings.TrimSuffix(file, ".sql"), sql: string(content)})
	}
	slices.SortFunc(all, func(a, b migration) int { return cmp.Compare(a.version, b.version) })
	return all, nil
}
