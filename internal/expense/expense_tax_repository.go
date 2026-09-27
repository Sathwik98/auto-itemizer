package expense

import (
	"context"
	"database/sql"
	"fmt"

	"auto-itemizer/internal/db"

	"github.com/google/uuid"
)

// SQL for the expense_tax table. Reads join tax_master for the tax's name.

// insertExpenseTax writes one tax row. The rate is stored exactly as parsed
// ("0.19", "0.09975"), and money with two decimals.
func insertExpenseTax(ctx context.Context, q db.Querier, expenseID, taxMasterID string, t Tax, now string) error {
	var taxable any // NULL when the receipt doesn't show it
	if t.TaxableAmount != nil {
		taxable = t.TaxableAmount.StringFixed(2)
	}
	_, err := q.ExecContext(ctx, `
		INSERT INTO expense_tax (id, expense_id, tax_master_id, rate, taxable_amount, tax_amount,
		                         created_at, created_by, updated_at, updated_by)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), expenseID, taxMasterID, t.Rate.String(), taxable, t.Amount.StringFixed(2),
		now, db.ActorSystem, now, db.ActorSystem)
	if err != nil {
		return fmt.Errorf("insert expense_tax: %w", err)
	}
	return nil
}

// selectExpenseTaxes reads an expense's taxes in the order they were inserted.
func selectExpenseTaxes(ctx context.Context, q db.Querier, expenseID string) ([]Tax, error) {
	rows, err := q.QueryContext(ctx, `
		SELECT m.name, t.rate, t.taxable_amount, t.tax_amount
		FROM expense_tax t
		JOIN tax_master m ON m.id = t.tax_master_id
		WHERE t.expense_id = ?
		ORDER BY t.rowid`, expenseID)
	if err != nil {
		return nil, fmt.Errorf("select taxes of expense %s: %w", expenseID, err)
	}
	defer rows.Close()

	var taxes []Tax
	for rows.Next() {
		var rate, amount string
		var taxable sql.NullString
		var tax Tax
		if err := rows.Scan(&tax.Name, &rate, &taxable, &amount); err != nil {
			return nil, fmt.Errorf("scan tax of expense %s: %w", expenseID, err)
		}
		if tax.Rate, err = decimalFrom("expense_tax.rate", rate); err != nil {
			return nil, err
		}
		if tax.Amount, err = decimalFrom("expense_tax.tax_amount", amount); err != nil {
			return nil, err
		}
		if taxable.Valid {
			v, err := decimalFrom("expense_tax.taxable_amount", taxable.String)
			if err != nil {
				return nil, err
			}
			tax.TaxableAmount = &v
		}
		taxes = append(taxes, tax)
	}
	return taxes, rows.Err()
}
