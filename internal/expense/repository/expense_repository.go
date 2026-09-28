// Package repository is the SQL for the expense tables (expense, expense_tax,
// expense_line_item and tax_master), one file per table. Only the expense
// packages may import it (internal/layout checks this), so ExpenseService
// stays the only code that writes these tables (ARCHITECTURE.md §1).
package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"auto-itemizer/internal/db"
	"auto-itemizer/internal/expense/core"

	"github.com/shopspring/decimal"
)

// SQL for the expense table.

func InsertExpense(ctx context.Context, q db.Querier, e core.Expense, now string) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO expense (id, receipt_id, itemization_status, merchant, date, currency, total,
		                     created_at, created_by, updated_at, updated_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		e.ID, e.ReceiptID, e.ItemizationStatus, e.Merchant, e.Date, e.Currency, e.Total.StringFixed(2),
		now, db.ActorSystem, now, db.ActorSystem)
	if err != nil {
		return fmt.Errorf("insert expense: %w", err)
	}
	return nil
}

// SelectActiveExpense reads the header of an active expense, or returns
// core.ErrNotFound.
func SelectActiveExpense(ctx context.Context, q db.Querier, id string) (core.Expense, error) {
	var e core.Expense
	var total string
	err := q.QueryRowContext(ctx, `
		SELECT id, receipt_id, itemization_status, merchant, date, currency, total, updated_at
		FROM expense
		WHERE id = ? AND is_deleted = 0`, id).
		Scan(&e.ID, &e.ReceiptID, &e.ItemizationStatus, &e.Merchant, &e.Date, &e.Currency, &total, &e.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Expense{}, core.ErrNotFound
	}
	if err != nil {
		return core.Expense{}, fmt.Errorf("select expense %s: %w", id, err)
	}
	if e.Total, err = decimalFrom("expense.total", total); err != nil {
		return core.Expense{}, err
	}
	return e, nil
}

// SelectActiveExpenseID returns the id of the receipt's active expense, or ""
// when it has none.
func SelectActiveExpenseID(ctx context.Context, q db.Querier, receiptID string) (string, error) {
	var id string
	err := q.QueryRowContext(ctx, `
		SELECT id FROM expense WHERE receipt_id = ? AND is_deleted = 0`, receiptID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("select active expense of receipt %s: %w", receiptID, err)
	}
	return id, nil
}

// SoftDeleteActiveExpense marks the receipt's active expense deleted, if it
// has one. Its taxes and items are left as they are (ARCHITECTURE.md §2).
func SoftDeleteActiveExpense(ctx context.Context, q db.Querier, receiptID, now string) error {
	_, err := q.ExecContext(ctx, `
		UPDATE expense SET is_deleted = 1, updated_at = ?, updated_by = ?
		WHERE receipt_id = ? AND is_deleted = 0`, now, db.ActorSystem, receiptID)
	if err != nil {
		return fmt.Errorf("soft-delete expense of receipt %s: %w", receiptID, err)
	}
	return nil
}

// UpdateItemizationStatus sets the status, but only if the expense is still
// active and unchanged since it was read (updated_at = readUpdatedAt).
// Otherwise it returns db.ErrConflict (ARCHITECTURE.md §3.0).
func UpdateItemizationStatus(ctx context.Context, q db.Querier, id, status, readUpdatedAt, actor string) error {
	res, err := q.ExecContext(ctx, `
		UPDATE expense SET itemization_status = ?, updated_at = ?, updated_by = ?
		WHERE id = ? AND updated_at = ? AND is_deleted = 0`,
		status, db.NextUpdatedAt(readUpdatedAt), actor, id, readUpdatedAt)
	if err != nil {
		return fmt.Errorf("update expense %s: %w", id, err)
	}
	return db.ExpectOneRow(res)
}

// decimalFrom parses a decimal stored as text. The service only ever writes
// decimal strings, so an error means the row was changed outside the service.
func decimalFrom(column, value string) (decimal.Decimal, error) {
	d, err := decimal.NewFromString(value)
	if err != nil {
		return decimal.Decimal{}, fmt.Errorf("%s %q is not a decimal: %w", column, value, err)
	}
	return d, nil
}
