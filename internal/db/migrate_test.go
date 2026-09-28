package db

import (
	"database/sql"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"testing/fstest"
)

// rawDB opens a database file without running any migration.
func rawDB(t *testing.T) *DB {
	t.Helper()
	sqlDB, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "test.db")+dsnParams)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	return &DB{sqlDB}
}

// applied returns the names recorded in schema_migrations, in version order.
func applied(t *testing.T, d *DB) []string {
	t.Helper()
	rows, err := d.QueryContext(ctx, `SELECT name FROM schema_migrations ORDER BY version`)
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
	return names
}

func tableExists(t *testing.T, d *DB, name string) bool {
	t.Helper()
	var n int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n == 1
}

var builtIn = []string{"0001_create_tables", "0002_seed_tax_names"}

// A fresh database gets every built-in migration once, and reopening it
// applies nothing again.
func TestOpenAppliesMigrationsOnce(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	first, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if got := applied(t, first); !slices.Equal(got, builtIn) {
		t.Errorf("applied = %v, want %v", got, builtIn)
	}
	var appliedAt string
	if err := first.QueryRowContext(ctx, `SELECT applied_at FROM schema_migrations WHERE version = 1`).Scan(&appliedAt); err != nil {
		t.Fatal(err)
	}
	first.Close()

	second, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	var again string
	if err := second.QueryRowContext(ctx, `SELECT applied_at FROM schema_migrations WHERE version = 1`).Scan(&again); err != nil {
		t.Fatal(err)
	}
	if got := applied(t, second); !slices.Equal(got, builtIn) || again != appliedAt {
		t.Errorf("after reopening: applied = %v at %s; want %v, still at %s", got, again, builtIn, appliedAt)
	}
}

// A database created by the old schema.sql (the same SQL, but no
// schema_migrations table) is adopted: the migrations are recorded, and its
// data is kept.
func TestOldDatabaseIsAdopted(t *testing.T) {
	d := rawDB(t)
	for _, name := range builtIn {
		content, err := migrationFiles.ReadFile("migrations/" + name + ".sql")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.ExecContext(ctx, string(content)); err != nil {
			t.Fatalf("old schema %s: %v", name, err)
		}
	}
	if _, err := d.ExecContext(ctx, `
		INSERT INTO file_upload (id, file_path, file_name, content_type, size_bytes, created_at, created_by, updated_at, updated_by)
		VALUES ('f1', 'storage/receipts/f1.txt', 'x.txt', 'text/plain', 1, 'now', 'system', 'now', 'system')`); err != nil {
		t.Fatal(err)
	}

	if err := migrate(ctx, d, migrationFiles); err != nil {
		t.Fatalf("migrate an old database: %v", err)
	}
	if got := applied(t, d); !slices.Equal(got, builtIn) {
		t.Errorf("applied = %v, want %v", got, builtIn)
	}
	var n int
	if err := d.QueryRowContext(ctx, `SELECT count(*) FROM file_upload`).Scan(&n); err != nil || n != 1 {
		t.Errorf("file_upload rows = %d (%v), want the 1 already there", n, err)
	}
}

// A failing migration stops with its file's name and leaves nothing behind:
// the migrations before it stay, and its own changes are rolled back.
func TestFailingMigrationIsRolledBack(t *testing.T) {
	d := rawDB(t)
	files := fstest.MapFS{
		"migrations/0001_create_a.sql": {Data: []byte(`CREATE TABLE a (x INTEGER);`)},
		"migrations/0002_broken.sql":   {Data: []byte(`CREATE TABLE b (x INTEGER); THIS IS NOT SQL;`)},
	}
	err := migrate(ctx, d, files)
	if err == nil || !strings.Contains(err.Error(), "0002_broken") {
		t.Fatalf("migrate = %v, want an error naming 0002_broken", err)
	}
	if got := applied(t, d); !slices.Equal(got, []string{"0001_create_a"}) {
		t.Errorf("applied = %v, want only 0001_create_a", got)
	}
	if !tableExists(t, d, "a") || tableExists(t, d, "b") {
		t.Errorf("table a exists: %v, table b exists: %v; want a kept and b rolled back", tableExists(t, d, "a"), tableExists(t, d, "b"))
	}
}

// A migration added later is applied on the next start, and only it.
func TestNewMigrationIsAppliedNextTime(t *testing.T) {
	d := rawDB(t)
	first := fstest.MapFS{"migrations/0001_create_a.sql": {Data: []byte(`CREATE TABLE a (x INTEGER);`)}}
	if err := migrate(ctx, d, first); err != nil {
		t.Fatal(err)
	}
	later := fstest.MapFS{
		"migrations/0001_create_a.sql": {Data: []byte(`CREATE TABLE a (x INTEGER);`)}, // would fail if run twice
		"migrations/0002_create_c.sql": {Data: []byte(`CREATE TABLE c (x INTEGER);`)},
	}
	if err := migrate(ctx, d, later); err != nil {
		t.Fatalf("migrate with a new file: %v", err)
	}
	if got := applied(t, d); !slices.Equal(got, []string{"0001_create_a", "0002_create_c"}) || !tableExists(t, d, "c") {
		t.Errorf("applied = %v, table c exists: %v", got, tableExists(t, d, "c"))
	}
}

// Migrations run in number order: 0010 needs the table that 0009 creates.
// (The 4-digit names make number order and name order the same.)
func TestMigrationsRunInNumberOrder(t *testing.T) {
	d := rawDB(t)
	files := fstest.MapFS{
		"migrations/0010_fill_t.sql":   {Data: []byte(`INSERT INTO t (x) VALUES (1);`)},
		"migrations/0009_create_t.sql": {Data: []byte(`CREATE TABLE t (x INTEGER);`)},
	}
	if err := migrate(ctx, d, files); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	if got := applied(t, d); !slices.Equal(got, []string{"0009_create_t", "0010_fill_t"}) {
		t.Errorf("applied = %v, want 0009 then 0010", got)
	}
}

// A badly named file, or two files with the same number, stops with the
// file's name instead of guessing an order.
func TestBadMigrationNames(t *testing.T) {
	sqlFile := &fstest.MapFile{Data: []byte(`SELECT 1;`)}
	for _, tc := range []struct {
		files fstest.MapFS
		want  string
	}{
		{fstest.MapFS{"migrations/1_create.sql": sqlFile}, "1_create.sql"},
		{fstest.MapFS{"migrations/0001-create.sql": sqlFile}, "0001-create.sql"},
		{fstest.MapFS{"migrations/0001_create.txt": sqlFile}, "0001_create.txt"},
		{fstest.MapFS{"migrations/0001_a.sql": sqlFile, "migrations/0001_b.sql": sqlFile}, "same number"},
	} {
		d := rawDB(t)
		if err := migrate(ctx, d, tc.files); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("migrate(%v) = %v, want an error about %q", tc.files, err, tc.want)
		}
	}
}
