package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"auto-itemizer/internal/db"
	"auto-itemizer/internal/expense"
	"auto-itemizer/internal/receipt"
)

// errorBody is the body of every 4xx and 5xx response (ARCHITECTURE.md §3.0).
// The last three fields are only set for ITEMS_DO_NOT_RECONCILE.
type errorBody struct {
	Error      string `json:"error"`
	Message    string `json:"message"`
	Expected   string `json:"expected,omitempty"`
	Actual     string `json:"actual,omitempty"`
	Difference string `json:"difference,omitempty"`
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		slog.Error("write response", "error", err)
	}
}

// writeProblem writes an error response with a code and a message.
func writeProblem(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Error: code, Message: message})
}

// writeError turns an error from a service into its response, using the
// table in IMPLEMENTATION_PLAN.md §6.
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	var guardErr *receipt.GuardError
	var mismatch *expense.MismatchError
	switch {
	case errors.Is(err, context.Canceled):
		// The client has gone, so there is no one to answer.
	case errors.Is(err, receipt.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "NOT_FOUND", "receipt not found")
	case errors.Is(err, expense.ErrNotFound):
		writeProblem(w, http.StatusNotFound, "NOT_FOUND", "transaction not found")
	case errors.Is(err, receipt.ErrLiveOCRNotConfigured):
		writeProblem(w, http.StatusNotImplemented, "LIVE_OCR_NOT_CONFIGURED",
			"live OCR is not configured; run with MOCK_OCR=true")
	case errors.As(err, &guardErr):
		writeProblem(w, http.StatusUnprocessableEntity, guardErr.Code, guardErr.Message)
	case errors.As(err, &mismatch):
		writeJSON(w, http.StatusConflict, errorBody{
			Error:      "ITEMS_DO_NOT_RECONCILE",
			Message:    "Items plus taxes do not equal the total",
			Expected:   mismatch.Expected.StringFixed(2),
			Actual:     mismatch.Actual.StringFixed(2),
			Difference: mismatch.Difference.StringFixed(2),
		})
	case errors.Is(err, db.ErrConflict):
		writeProblem(w, http.StatusConflict, "CONFLICT", "another request changed this first; try again")
	case errors.Is(err, expense.ErrUnknownItem):
		writeProblem(w, http.StatusBadRequest, "UNKNOWN_ITEM", err.Error())
	case errors.Is(err, expense.ErrInvalidItem):
		writeProblem(w, http.StatusBadRequest, "INVALID_ITEM", err.Error())
	default:
		// Log the details, but don't send internals to the client.
		slog.Error("request failed", "method", r.Method, "path", r.URL.Path, "error", err)
		writeProblem(w, http.StatusInternalServerError, "INTERNAL_ERROR", "internal error")
	}
}
