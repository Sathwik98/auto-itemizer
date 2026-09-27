// Package repository is the SQL for the receipt tables (receipts and
// receipt_ocr), one file per table. Only the receipt packages may import it
// (internal/layout checks this), so ReceiptService stays the only code that
// writes these tables (ARCHITECTURE.md §1).
package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"auto-itemizer/internal/db"
	"auto-itemizer/internal/receipt/core"
)

// SQL for the receipts table.

func InsertReceipt(ctx context.Context, q db.Querier, r core.Receipt) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO receipts (id, file_id, status, created_at, created_by, updated_at, updated_by)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.FileID, r.Status, r.CreatedAt, db.ActorSystem, r.UpdatedAt, db.ActorSystem)
	if err != nil {
		return fmt.Errorf("insert receipt: %w", err)
	}
	return nil
}

// SelectReceipt reads a receipt, or returns core.ErrNotFound.
func SelectReceipt(ctx context.Context, q db.Querier, id string) (core.Receipt, error) {
	var r core.Receipt
	var reason sql.NullString
	err := q.QueryRowContext(ctx, `
		SELECT id, file_id, status, failure_reason, created_at, updated_at
		FROM receipts
		WHERE id = ?`, id).
		Scan(&r.ID, &r.FileID, &r.Status, &reason, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Receipt{}, core.ErrNotFound
	}
	if err != nil {
		return core.Receipt{}, fmt.Errorf("select receipt %s: %w", id, err)
	}
	r.FailureReason = reason.String // "" when NULL
	return r, nil
}

// UpdateStatus moves the receipt to status with failureReason ("" for none),
// but only if it is unchanged since it was read (updated_at = r.UpdatedAt).
// Otherwise it returns db.ErrConflict (ARCHITECTURE.md §3.0). It returns the
// receipt as written.
func UpdateStatus(ctx context.Context, q db.Querier, r core.Receipt, status, failureReason string) (core.Receipt, error) {
	var reason any // NULL when there is none
	if failureReason != "" {
		reason = failureReason
	}
	updatedAt := db.NextUpdatedAt(r.UpdatedAt)
	res, err := q.ExecContext(ctx, `
		UPDATE receipts SET status = ?, failure_reason = ?, updated_at = ?, updated_by = ?
		WHERE id = ? AND updated_at = ?`,
		status, reason, updatedAt, db.ActorSystem, r.ID, r.UpdatedAt)
	if err != nil {
		return core.Receipt{}, fmt.Errorf("update receipt %s: %w", r.ID, err)
	}
	if err := db.ExpectOneRow(res); err != nil {
		return core.Receipt{}, err
	}
	r.Status, r.FailureReason, r.UpdatedAt = status, failureReason, updatedAt
	return r, nil
}
