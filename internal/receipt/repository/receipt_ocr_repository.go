package repository

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"auto-itemizer/internal/db"
	"auto-itemizer/internal/receipt/core"

	"github.com/google/uuid"
)

// SQL for the receipt_ocr table. Each OCR run adds a row and soft-deletes the
// previous one, so a receipt has one active row and keeps its history
// (ARCHITECTURE.md §2).

// SoftDeleteActiveOCR marks the receipt's active OCR row deleted, if it has one.
func SoftDeleteActiveOCR(ctx context.Context, q db.Querier, receiptID, now string) error {
	_, err := q.ExecContext(ctx, `
		UPDATE receipt_ocr SET is_deleted = 1, updated_at = ?, updated_by = ?
		WHERE receipt_id = ? AND is_deleted = 0`, now, db.ActorSystem, receiptID)
	if err != nil {
		return fmt.Errorf("soft-delete OCR of receipt %s: %w", receiptID, err)
	}
	return nil
}

// InsertOCR stores the raw OCR text exactly as the provider returned it.
func InsertOCR(ctx context.Context, q db.Querier, receiptID, payload, now string) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO receipt_ocr (id, receipt_id, ocr_payload, created_at, created_by, updated_at, updated_by)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), receiptID, payload, now, db.ActorSystem, now, db.ActorSystem)
	if err != nil {
		return fmt.Errorf("insert OCR of receipt %s: %w", receiptID, err)
	}
	return nil
}

// SelectActiveOCR returns the receipt's active OCR text, or core.ErrNoOCR.
func SelectActiveOCR(ctx context.Context, q db.Querier, receiptID string) (string, error) {
	var payload string
	err := q.QueryRowContext(ctx, `
		SELECT ocr_payload FROM receipt_ocr WHERE receipt_id = ? AND is_deleted = 0`, receiptID).
		Scan(&payload)
	if errors.Is(err, sql.ErrNoRows) {
		return "", fmt.Errorf("%w: receipt %s", core.ErrNoOCR, receiptID)
	}
	if err != nil {
		return "", fmt.Errorf("select OCR of receipt %s: %w", receiptID, err)
	}
	return payload, nil
}
