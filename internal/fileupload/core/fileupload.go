// Package core holds the FileUpload type. Like every core package it has no
// database code (internal/layout checks this).
package core

// FileUpload is one row of the file_upload table.
type FileUpload struct {
	ID          string
	FilePath    string // where the file was saved, e.g. storage/receipts/<id>.png
	FileName    string // name as uploaded; stored only, never used to build a path
	ContentType string
	SizeBytes   int64
	CreatedAt   string
	UpdatedAt   string
}
