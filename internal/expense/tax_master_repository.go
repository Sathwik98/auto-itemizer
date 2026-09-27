package expense

import (
	"context"
	"fmt"

	"auto-itemizer/internal/db"

	"github.com/google/uuid"
)

// SQL for the tax_master table. Its names are also the parser's list of tax
// names, loaded at startup (ARCHITECTURE.md §2).

// upsertTaxName returns the id of a tax name, adding the name first if it is
// new. A name is added once, however many receipts print it.
func upsertTaxName(ctx context.Context, q db.Querier, name, now string) (string, error) {
	_, err := q.ExecContext(ctx, `
		INSERT INTO tax_master (id, name, created_at, created_by, updated_at, updated_by)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (name) DO NOTHING`,
		uuid.NewString(), name, now, db.ActorSystem, now, db.ActorSystem)
	if err != nil {
		return "", fmt.Errorf("insert tax_master %q: %w", name, err)
	}
	var id string
	if err := q.QueryRowContext(ctx, `SELECT id FROM tax_master WHERE name = ?`, name).Scan(&id); err != nil {
		return "", fmt.Errorf("select tax_master %q: %w", name, err)
	}
	return id, nil
}

// selectTaxNames returns every tax name, in alphabetical order.
func selectTaxNames(ctx context.Context, q db.Querier) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT name FROM tax_master ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("select tax names: %w", err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("scan tax name: %w", err)
		}
		names = append(names, name)
	}
	return names, rows.Err()
}
