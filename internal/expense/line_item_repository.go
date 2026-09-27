package expense

import (
	"context"
	"fmt"

	"auto-itemizer/internal/db"

	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

// SQL for the expense_line_item table. Items are never updated in place: a
// change soft-deletes the old row and inserts a new one, so the history is
// kept (ARCHITECTURE.md §3.6).

// insertLineItem writes a new item and returns it with its new id.
func insertLineItem(ctx context.Context, q db.Querier, expenseID, description string,
	amount decimal.Decimal, actor, now string) (LineItem, error) {
	item := LineItem{ID: uuid.NewString(), Description: description, Amount: amount}
	_, err := q.ExecContext(ctx, `
		INSERT INTO expense_line_item (id, expense_id, name, amount,
		                               created_at, created_by, updated_at, updated_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		item.ID, expenseID, description, amount.StringFixed(2), now, actor, now, actor)
	if err != nil {
		return LineItem{}, fmt.Errorf("insert expense_line_item: %w", err)
	}
	return item, nil
}

// selectActiveLineItems reads an expense's active items in the order they
// were inserted (ORDER BY rowid, IMPLEMENTATION_PLAN.md §3).
func selectActiveLineItems(ctx context.Context, q db.Querier, expenseID string) ([]LineItem, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT id, name, amount
		FROM expense_line_item
		WHERE expense_id = ? AND is_deleted = 0
		ORDER BY rowid`, expenseID)
	if err != nil {
		return nil, fmt.Errorf("select items of expense %s: %w", expenseID, err)
	}
	defer rows.Close()

	var items []LineItem
	for rows.Next() {
		var item LineItem
		var amount string
		if err := rows.Scan(&item.ID, &item.Description, &amount); err != nil {
			return nil, fmt.Errorf("scan item of expense %s: %w", expenseID, err)
		}
		if item.Amount, err = decimalFrom("expense_line_item.amount", amount); err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, rows.Err()
}

// softDeleteLineItem marks one item deleted.
func softDeleteLineItem(ctx context.Context, q db.Querier, id, actor, now string) error {
	_, err := q.ExecContext(ctx, `
		UPDATE expense_line_item SET is_deleted = 1, updated_at = ?, updated_by = ?
		WHERE id = ? AND is_deleted = 0`, now, actor, id)
	if err != nil {
		return fmt.Errorf("soft-delete item %s: %w", id, err)
	}
	return nil
}

// softDeleteActiveLineItems marks all of an expense's active items deleted.
func softDeleteActiveLineItems(ctx context.Context, q db.Querier, expenseID, actor, now string) error {
	_, err := q.ExecContext(ctx, `
		UPDATE expense_line_item SET is_deleted = 1, updated_at = ?, updated_by = ?
		WHERE expense_id = ? AND is_deleted = 0`, now, actor, expenseID)
	if err != nil {
		return fmt.Errorf("soft-delete items of expense %s: %w", expenseID, err)
	}
	return nil
}
