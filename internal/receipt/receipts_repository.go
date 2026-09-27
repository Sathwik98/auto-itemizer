package receipt

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"auto-itemizer/internal/db"
)

// SQL for the receipts table.

func insertReceipt(ctx context.Context, q db.Querier, r Receipt) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO receipts (id, file_id, status, created_at, created_by, updated_at, updated_by)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		r.ID, r.FileID, r.Status, r.CreatedAt, db.ActorSystem, r.UpdatedAt, db.ActorSystem)
	if err != nil {
		return fmt.Errorf("insert receipt: %w", err)
	}
	return nil
}

// selectReceipt reads a receipt, or returns ErrNotFound.
func selectReceipt(ctx context.Context, q db.Querier, id string) (Receipt, error) {
	var r Receipt
	var reason sql.NullString
	err := q.QueryRowContext(ctx, `
		SELECT id, file_id, status, failure_reason, created_at, updated_at
		FROM receipts
		WHERE id = ?`, id).
		Scan(&r.ID, &r.FileID, &r.Status, &reason, &r.CreatedAt, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return Receipt{}, ErrNotFound
	}
	if err != nil {
		return Receipt{}, fmt.Errorf("select receipt %s: %w", id, err)
	}
	r.FailureReason = reason.String // "" when NULL
	return r, nil
}

// updateStatus moves the receipt to status with failureReason ("" for none),
// but only if it is unchanged since it was read (updated_at = r.UpdatedAt).
// Otherwise it returns db.ErrConflict (ARCHITECTURE.md §3.0). It returns the
// receipt as written.
func updateStatus(ctx context.Context, q db.Querier, r Receipt, status, failureReason string) (Receipt, error) {
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
		return Receipt{}, fmt.Errorf("update receipt %s: %w", r.ID, err)
	}
	if err := db.ExpectOneRow(res); err != nil {
		return Receipt{}, err
	}
	r.Status, r.FailureReason, r.UpdatedAt = status, failureReason, updatedAt
	return r, nil
}
