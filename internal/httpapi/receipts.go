package httpapi

import (
	"errors"
	"mime"
	"net/http"
	"strings"

	"auto-itemizer/internal/receipt"
)

// receiptController is the ReceiptController (ARCHITECTURE.md §1): HTTP for
// POST /receipts, POST /receipts/{id}/process and GET /receipts/{id}.
type receiptController struct {
	receipts *receipt.Service
}

const (
	maxUploadBytes = 10 << 20               // largest accepted file: 10 MB (ARCHITECTURE.md §3.1)
	maxUploadBody  = maxUploadBytes + 1<<20 // plus room for the multipart envelope
)

// upload handles POST /receipts: a multipart form with the receipt in the
// field "file".
func (c *receiptController) upload(w http.ResponseWriter, r *http.Request) {
	// Read the whole upload before any database work: the upload transaction
	// holds the write lock while the file is saved (IMPLEMENTATION_PLAN.md §6).
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBody)
	file, header, err := r.FormFile("file")
	if err != nil {
		var tooBig *http.MaxBytesError
		switch {
		case errors.As(err, &tooBig):
			writeProblem(w, http.StatusRequestEntityTooLarge, "FILE_TOO_LARGE", "the file must be at most 10 MB")
		case errors.Is(err, http.ErrMissingFile):
			writeProblem(w, http.StatusBadRequest, "FILE_MISSING", `send the receipt in a multipart form field named "file"`)
		default:
			writeProblem(w, http.StatusBadRequest, "INVALID_UPLOAD", "the request must be multipart/form-data with a file field")
		}
		return
	}
	defer file.Close()

	if header.Size > maxUploadBytes {
		writeProblem(w, http.StatusRequestEntityTooLarge, "FILE_TOO_LARGE", "the file must be at most 10 MB")
		return
	}
	if header.Size == 0 {
		writeProblem(w, http.StatusBadRequest, "FILE_EMPTY", "the file is empty")
		return
	}
	// "text/plain; charset=utf-8" becomes "text/plain": the stored file's
	// extension is looked up by the bare type.
	mediaType, _, err := mime.ParseMediaType(header.Header.Get("Content-Type"))
	if err != nil || !allowedType(mediaType) {
		writeProblem(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_FILE_TYPE",
			"the file must be an image, a PDF or plain text")
		return
	}

	rec, err := c.receipts.Upload(r.Context(), header.Filename, mediaType, file)
	if err != nil {
		writeError(w, r, err)
		return
	}
	w.Header().Set("Location", "/receipts/"+rec.ID)
	writeJSON(w, http.StatusCreated, uploadView{ReceiptID: rec.ID, Status: rec.Status})
}

// allowedType reports whether a receipt may have this media type
// (ARCHITECTURE.md §3.1). text/plain is allowed so the fixture .txt files can
// be uploaded.
func allowedType(mediaType string) bool {
	return strings.HasPrefix(mediaType, "image/") || mediaType == "application/pdf" || mediaType == "text/plain"
}

// process handles POST /receipts/{id}/process.
func (c *receiptController) process(w http.ResponseWriter, r *http.Request) {
	e, err := c.receipts.Process(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newExpenseView(e))
}

// get handles GET /receipts/{id}.
func (c *receiptController) get(w http.ResponseWriter, r *http.Request) {
	d, err := c.receipts.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newReceiptView(d))
}
