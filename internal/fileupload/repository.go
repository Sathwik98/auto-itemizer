package fileupload

import (
	"context"
	"fmt"

	"auto-itemizer/internal/db"
)

// SQL for the file_upload table, the only table this package writes.

func insertFileUpload(ctx context.Context, q db.Querier, f FileUpload) error {
	_, err := q.ExecContext(ctx, `
		INSERT INTO file_upload (id, file_path, file_name, content_type, size_bytes,
		                         created_at, created_by, updated_at, updated_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		f.ID, f.FilePath, f.FileName, f.ContentType, f.SizeBytes,
		f.CreatedAt, db.ActorSystem, f.UpdatedAt, db.ActorSystem)
	if err != nil {
		return fmt.Errorf("insert file_upload: %w", err)
	}
	return nil
}

func selectFileUpload(ctx context.Context, q db.Querier, id string) (FileUpload, error) {
	var f FileUpload
	err := q.QueryRowContext(ctx, `
		SELECT id, file_path, file_name, content_type, size_bytes, created_at, updated_at
		FROM file_upload
		WHERE id = ?`, id).
		Scan(&f.ID, &f.FilePath, &f.FileName, &f.ContentType, &f.SizeBytes, &f.CreatedAt, &f.UpdatedAt)
	return f, err
}
