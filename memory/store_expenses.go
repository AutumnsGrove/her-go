package memory

import (
	"fmt"
	"strings"
	"time"
)

// --- Expense Tracking ---

// ExpenseItem represents an individual line item from a receipt.
// Linked to a parent Expense via ExpenseID. Stored in expense_items table.
type ExpenseItem struct {
	ID          int64
	ExpenseID   int64
	Description string
	Quantity    int
	UnitPrice   float64
	TotalPrice  float64
}

// Expense represents a single expense entry from receipt scanning or manual input.
// This data is intentionally separate from facts — financial transactions are not
// "memories" and should never end up in the facts table.
type Expense struct {
	ID              int64
	Amount          float64
	Currency        string
	Vendor          string
	Category        string
	Date            string // YYYY-MM-DD format
	Note            string
	SourceMessageID int64
	CreatedAt       time.Time
}

// SaveExpense inserts a new expense record and returns its ID.
// Called by the scan_receipt agent tool after the agent parses OCR text
// (or a manual expense mention) into structured fields.
//
// Same pattern as SaveMoodEntry — validate inputs, insert, return ID.
// The category validation happens in the agent tool handler, not here,
// since the store layer is intentionally dumb about business logic.
func (s *Store) SaveExpense(amount float64, currency, vendor, category, date, note string, sourceMessageID int64) (int64, error) {
	if currency == "" {
		currency = "USD"
	}

	// Handle nullable source_message_id — same pattern as SaveMetric.
	// In Go, interface{} (now called 'any') can hold any value including nil.
	// SQL drivers treat nil as NULL. So we convert 0 → nil for the FK column.
	var srcID interface{} = sourceMessageID
	if sourceMessageID == 0 {
		srcID = nil
	}

	result, err := s.db.Exec(
		`INSERT INTO expenses (amount, currency, vendor, category, date, note, source_message_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		amount, currency, vendor, category, date, note, srcID,
	)
	if err != nil {
		return 0, fmt.Errorf("saving expense: %w", err)
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("getting expense ID: %w", err)
	}
	return id, nil
}

// SaveExpenseItem inserts a line item linked to a parent expense.
// Called in a loop after SaveExpense when the agent extracts individual
// items from receipt OCR text.
func (s *Store) SaveExpenseItem(expenseID int64, description string, quantity int, unitPrice, totalPrice float64) error {
	if quantity < 1 {
		quantity = 1
	}
	_, err := s.db.Exec(
		`INSERT INTO expense_items (expense_id, description, quantity, unit_price, total_price)
		 VALUES (?, ?, ?, ?, ?)`,
		expenseID, description, quantity, unitPrice, totalPrice,
	)
	if err != nil {
		return fmt.Errorf("saving expense item: %w", err)
	}
	return nil
}

// DeleteExpense removes an expense and all its line items.
// Uses a transaction so both deletes succeed or neither does.
func (s *Store) DeleteExpense(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("starting transaction: %w", err)
	}
	// Delete child items first (foreign key), then parent expense.
	if _, err := tx.Exec(`DELETE FROM expense_items WHERE expense_id = ?`, id); err != nil {
		tx.Rollback()
		return fmt.Errorf("deleting expense items: %w", err)
	}
	result, err := tx.Exec(`DELETE FROM expenses WHERE id = ?`, id)
	if err != nil {
		tx.Rollback()
		return fmt.Errorf("deleting expense: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		tx.Rollback()
		return fmt.Errorf("expense ID=%d not found", id)
	}
	return tx.Commit()
}

// UpdateExpense modifies fields on an existing expense. Only non-zero/non-empty
// values are updated — pass zero/empty to leave a field unchanged.
func (s *Store) UpdateExpense(id int64, amount float64, currency, vendor, category, date, note string) error {
	// Build SET clause dynamically — only include fields that have values.
	var sets []string
	var args []interface{}

	if amount > 0 {
		sets = append(sets, "amount = ?")
		args = append(args, amount)
	}
	if currency != "" {
		sets = append(sets, "currency = ?")
		args = append(args, currency)
	}
	if vendor != "" {
		sets = append(sets, "vendor = ?")
		args = append(args, vendor)
	}
	if category != "" {
		sets = append(sets, "category = ?")
		args = append(args, category)
	}
	if date != "" {
		sets = append(sets, "date = ?")
		args = append(args, date)
	}
	if note != "" {
		sets = append(sets, "note = ?")
		args = append(args, note)
	}

	if len(sets) == 0 {
		return fmt.Errorf("no fields to update")
	}

	query := fmt.Sprintf("UPDATE expenses SET %s WHERE id = ?", strings.Join(sets, ", "))
	args = append(args, id)

	result, err := s.db.Exec(query, args...)
	if err != nil {
		return fmt.Errorf("updating expense: %w", err)
	}
	rows, _ := result.RowsAffected()
	if rows == 0 {
		return fmt.Errorf("expense ID=%d not found", id)
	}
	return nil
}

// RecentExpenses returns the last N expenses with their line items, newest first.
// Used by the query_expenses tool to answer financial questions.
func (s *Store) RecentExpenses(limit int) ([]Expense, map[int64][]ExpenseItem, error) {
	rows, err := s.db.Query(
		`SELECT id, amount, COALESCE(currency, 'USD'), COALESCE(vendor, ''),
		        category, date, COALESCE(note, ''), COALESCE(source_message_id, 0),
		        created_at
		 FROM expenses
		 ORDER BY date DESC, created_at DESC
		 LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("querying expenses: %w", err)
	}
	defer rows.Close()

	var expenses []Expense
	for rows.Next() {
		var e Expense
		var ts string
		if err := rows.Scan(&e.ID, &e.Amount, &e.Currency, &e.Vendor,
			&e.Category, &e.Date, &e.Note, &e.SourceMessageID, &ts); err != nil {
			return nil, nil, fmt.Errorf("scanning expense: %w", err)
		}
		e.CreatedAt, _ = time.Parse("2006-01-02 15:04:05", ts)
		expenses = append(expenses, e)
	}

	// Fetch line items for all returned expenses.
	items := make(map[int64][]ExpenseItem)
	for _, e := range expenses {
		itemRows, err := s.db.Query(
			`SELECT id, expense_id, description, quantity,
			        COALESCE(unit_price, 0), COALESCE(total_price, 0)
			 FROM expense_items WHERE expense_id = ?`,
			e.ID,
		)
		if err != nil {
			continue // non-fatal — expense still useful without items
		}
		for itemRows.Next() {
			var item ExpenseItem
			if err := itemRows.Scan(&item.ID, &item.ExpenseID, &item.Description,
				&item.Quantity, &item.UnitPrice, &item.TotalPrice); err != nil {
				continue
			}
			items[e.ID] = append(items[e.ID], item)
		}
		itemRows.Close()
	}

	return expenses, items, nil
}

// ExpenseSummary returns aggregate stats for expenses in a date range.
// Used by the query_expenses tool for "how much this week/month" questions.
func (s *Store) ExpenseSummary(startDate, endDate string) (total float64, byCategory map[string]float64, count int, err error) {
	byCategory = make(map[string]float64)

	rows, err := s.db.Query(
		`SELECT category, SUM(amount), COUNT(*)
		 FROM expenses
		 WHERE date >= ? AND date <= ?
		 GROUP BY category`,
		startDate, endDate,
	)
	if err != nil {
		return 0, nil, 0, fmt.Errorf("querying expense summary: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var cat string
		var sum float64
		var cnt int
		if err := rows.Scan(&cat, &sum, &cnt); err != nil {
			continue
		}
		byCategory[cat] = sum
		total += sum
		count += cnt
	}

	return total, byCategory, count, nil
}
