// Package server is the ExpenseController (ARCHITECTURE.md §1): HTTP for
// GET /transactions/{id}, POST /transactions/{id}/itemize and
// PATCH /transactions/{id}/items. The brief calls an expense a "transaction".
// It holds no business logic.
package server

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"auto-itemizer/internal/expense/core"
	"auto-itemizer/internal/expense/service"
	"auto-itemizer/internal/httpapi/models"
	"auto-itemizer/internal/httpapi/respond"
)

// Controller is the ExpenseController.
type Controller struct {
	expenses *service.Service
}

// New returns a Controller that calls expenses.
func New(expenses *service.Service) *Controller {
	return &Controller{expenses: expenses}
}

// maxPatchBody is the largest PATCH body accepted: 1 MB.
const maxPatchBody = 1 << 20

// Get handles GET /transactions/{id}.
func (c *Controller) Get(w http.ResponseWriter, r *http.Request) {
	e, err := c.expenses.Get(r.Context(), r.PathValue("id"))
	if err != nil {
		respond.WriteError(w, r, err)
		return
	}
	respond.WriteJSON(w, http.StatusOK, models.NewExpenseResponse(e))
}

// Itemize handles POST /transactions/{id}/itemize.
func (c *Controller) Itemize(w http.ResponseWriter, r *http.Request) {
	e, err := c.expenses.Reitemize(r.Context(), r.PathValue("id"))
	if err != nil {
		respond.WriteError(w, r, err)
		return
	}
	respond.WriteJSON(w, http.StatusOK, models.NewExpenseResponse(e))
}

// PatchItems handles PATCH /transactions/{id}/items. The body is the complete
// list of items the user wants (ARCHITECTURE.md §3.6).
func (c *Controller) PatchItems(w http.ResponseWriter, r *http.Request) {
	items, err := decodeItems(http.MaxBytesReader(w, r.Body, maxPatchBody))
	if err != nil {
		respond.WriteProblem(w, http.StatusBadRequest, "INVALID_BODY", err.Error())
		return
	}
	e, err := c.expenses.PatchItems(r.Context(), r.PathValue("id"), items)
	if err != nil {
		respond.WriteError(w, r, err)
		return
	}
	respond.WriteJSON(w, http.StatusOK, models.NewExpenseResponse(e))
}

// decodeItems reads a PATCH body: one JSON array of items and nothing after
// it. Unknown fields are refused, so a field we don't support (such as
// tax_amount) isn't silently dropped. The item rules themselves (description,
// amount, ids) are checked by ExpenseService.
func decodeItems(body io.Reader) ([]core.ItemInput, error) {
	dec := json.NewDecoder(body)
	dec.DisallowUnknownFields()

	var raw []models.ItemRequest
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("the body must be a JSON array of {id, description, amount}: %v", err)
	}
	if raw == nil {
		return nil, errors.New("the body must be a JSON array of {id, description, amount}, not null")
	}
	if err := dec.Decode(new(json.RawMessage)); !errors.Is(err, io.EOF) {
		return nil, errors.New("the body must contain only the JSON array")
	}

	items := make([]core.ItemInput, len(raw))
	for i, item := range raw {
		items[i] = core.ItemInput{ID: item.ID, Description: item.Description, Amount: item.Amount}
	}
	return items, nil
}
