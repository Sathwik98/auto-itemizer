package httpapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"auto-itemizer/internal/expense"

	"github.com/shopspring/decimal"
)

// expenseController is the ExpenseController (ARCHITECTURE.md §1): HTTP for
// GET /transactions/{id}, POST /transactions/{id}/itemize and
// PATCH /transactions/{id}/items. The brief calls an expense a "transaction".
type expenseController struct {
	expenses *expense.Service
}

// maxPatchBody is the largest PATCH body accepted: 1 MB.
const maxPatchBody = 1 << 20

// get handles GET /transactions/{id}.
func (c *expenseController) get(w http.ResponseWriter, r *http.Request) {
	e, err := c.expenses.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newExpenseView(e))
}

// itemize handles POST /transactions/{id}/itemize.
func (c *expenseController) itemize(w http.ResponseWriter, r *http.Request) {
	e, err := c.expenses.Reitemize(r.Context(), r.PathValue("id"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newExpenseView(e))
}

// patchItems handles PATCH /transactions/{id}/items. The body is the complete
// list of items the user wants (ARCHITECTURE.md §3.6).
func (c *expenseController) patchItems(w http.ResponseWriter, r *http.Request) {
	items, err := decodeItems(http.MaxBytesReader(w, r.Body, maxPatchBody))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}
	e, err := c.expenses.PatchItems(r.Context(), r.PathValue("id"), items)
	if err != nil {
		writeError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, newExpenseView(e))
}

// itemBody is one item of a PATCH body. The amount may be sent as "6.60" or
// 6.60; decimal.Decimal reads both exactly.
type itemBody struct {
	ID          string          `json:"id"`
	Description string          `json:"description"`
	Amount      decimal.Decimal `json:"amount"`
}

// decodeItems reads a PATCH body: one JSON array of items and nothing after
// it. Unknown fields are refused, so a field we don't support (such as
// tax_amount) isn't silently dropped. The item rules themselves (description,
// amount, ids) are checked by ExpenseService.
func decodeItems(body io.Reader) ([]expense.ItemInput, error) {
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()

	var raw []itemBody
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("the body must be a JSON array of {id, description, amount}: %v", err)
	}
	if raw == nil {
		return nil, errors.New("the body must be a JSON array of {id, description, amount}, not null")
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return nil, errors.New("the body must contain only the JSON array")
	}

	items := make([]expense.ItemInput, len(raw))
	for i, item := range raw {
		items[i] = expense.ItemInput{ID: item.ID, Description: item.Description, Amount: item.Amount}
	}
	return items, nil
}
