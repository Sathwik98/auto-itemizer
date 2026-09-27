// Package respond writes the API's responses: JSON bodies, and the mapping
// from a service error to its status code and error body (ARCHITECTURE.md
// §3.0). Both controllers and the router use it.
package respond

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"auto-itemizer/internal/db"
	expensecore "auto-itemizer/internal/expense/core"
	"auto-itemizer/internal/httpapi/models"
	receiptcore "auto-itemizer/internal/receipt/core"
)

// WriteJSON writes body as JSON with the given status code.
func WriteJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("write response", "error", err)
	}
}

// WriteProblem writes an error response with a code and a message.
func WriteProblem(w http.ResponseWriter, status int, code, message string) {
	WriteJSON(w, status, models.ErrorResponse{Error: code, Message: message})
}

// WriteError turns an error from a service into its response, using the
// table in IMPLEMENTATION_PLAN.md §6.
func WriteError(w http.ResponseWriter, r *http.Request, err error) {
	var guardErr *receiptcore.GuardError
	var mismatch *expensecore.MismatchError
	switch {
	case errors.Is(err, context.Canceled):
		// The client has gone, so there is no one to answer.
	case errors.Is(err, receiptcore.ErrNotFound):
		WriteProblem(w, http.StatusNotFound, "NOT_FOUND", "receipt not found")
	case errors.Is(err, expensecore.ErrNotFound):
		WriteProblem(w, http.StatusNotFound, "NOT_FOUND", "transaction not found")
	case errors.Is(err, receiptcore.ErrLiveOCRNotConfigured):
		WriteProblem(w, http.StatusNotImplemented, "LIVE_OCR_NOT_CONFIGURED",
			"live OCR is not configured; run with MOCK_OCR=true")
	case errors.As(err, &guardErr):
		WriteProblem(w, http.StatusUnprocessableEntity, guardErr.Code, guardErr.Message)
	case errors.As(err, &mismatch):
		WriteJSON(w, http.StatusConflict, models.ErrorResponse{
			Error:      "ITEMS_DO_NOT_RECONCILE",
			Message:    "Items plus taxes do not equal the total",
			Expected:   mismatch.Expected.StringFixed(2),
			Actual:     mismatch.Actual.StringFixed(2),
			Difference: mismatch.Difference.StringFixed(2),
		})
	case errors.Is(err, db.ErrConflict):
		WriteProblem(w, http.StatusConflict, "CONFLICT", "another request changed this first; try again")
	case errors.Is(err, expensecore.ErrUnknownItem):
		WriteProblem(w, http.StatusBadRequest, "UNKNOWN_ITEM", err.Error())
	case errors.Is(err, expensecore.ErrInvalidItem):
		WriteProblem(w, http.StatusBadRequest, "INVALID_ITEM", err.Error())
	default:
		// Log the details, but don't send internals to the client.
		slog.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		WriteProblem(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
	}
}
