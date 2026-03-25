// Package agent — expense tracking tool handlers (scan_receipt, query_expenses).
//
// This file implements tools for saving and querying financial data.
// The key design principle: financial data stays OUT of the facts table.
// Individual transactions go in the expenses/expense_items tables; only
// high-level patterns ("user is budgeting carefully") are allowed as facts.
package agent

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// validExpenseCategories is the fixed set of categories the agent can assign.
// This is enforced both in the tool schema (enum) and validated here in code.
// New categories can be added here — the tool schema in tools.go must match.
//
// Go doesn't have a Set type like Python. The idiomatic approach is a
// map[string]bool — check membership with validExpenseCategories["groceries"].
var validExpenseCategories = map[string]bool{
	"groceries":     true,
	"dining":        true,
	"coffee":        true,
	"transport":     true,
	"shopping":      true,
	"entertainment": true,
	"health":        true,
	"utilities":     true,
	"housing":       true,
	"subscriptions": true,
	"personal_care": true,
	"other":         true,
}

// datePattern validates YYYY-MM-DD format. Compiled once at package init
// (same pattern as the envVarPattern in config.go).
var datePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// lineItem is the JSON structure for individual receipt items passed
// by the agent. Parsed from the "items" array in scan_receipt args.
type lineItem struct {
	Description string  `json:"description"`
	Quantity    int     `json:"quantity"`
	UnitPrice   float64 `json:"unit_price"`
	TotalPrice  float64 `json:"total_price"`
}

// execScanReceipt handles the scan_receipt tool call. The agent provides
// structured expense data parsed from OCR text (or from a manual mention
// like "I spent $20 on lunch"). This function validates the data and
// saves it to the expenses table, along with any line items.
//
// Parameters (from agent):
//   - amount:   float, total amount (required)
//   - vendor:   string, store/restaurant name (required)
//   - category: enum, from preset list (required)
//   - date:     string, YYYY-MM-DD (required)
//   - currency: string, ISO 4217 code (optional, defaults to "USD")
//   - note:     string, optional context about the purchase
//   - items:    array of {description, quantity, unit_price, total_price}
func execScanReceipt(argsJSON string, tctx *toolContext) string {
	var args struct {
		Amount   float64    `json:"amount"`
		Vendor   string     `json:"vendor"`
		Category string     `json:"category"`
		Date     string     `json:"date"`
		Currency string     `json:"currency"`
		Note     string     `json:"note"`
		Items    []lineItem `json:"items"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	// --- Validation ---

	// Amount must be positive.
	if args.Amount <= 0 {
		return "error: amount must be greater than 0"
	}

	// Vendor is required.
	args.Vendor = strings.TrimSpace(args.Vendor)
	if args.Vendor == "" {
		return "error: vendor name is required"
	}

	// Category must be from the preset list.
	args.Category = strings.ToLower(strings.TrimSpace(args.Category))
	if !validExpenseCategories[args.Category] {
		valid := make([]string, 0, len(validExpenseCategories))
		for k := range validExpenseCategories {
			valid = append(valid, k)
		}
		return fmt.Sprintf("error: invalid category %q — valid options: %s", args.Category, strings.Join(valid, ", "))
	}

	// Date must be YYYY-MM-DD format.
	args.Date = strings.TrimSpace(args.Date)
	if args.Date == "" {
		// Default to today if the receipt date isn't visible.
		args.Date = time.Now().Format("2006-01-02")
	} else if !datePattern.MatchString(args.Date) {
		return fmt.Sprintf("error: date must be YYYY-MM-DD format, got %q", args.Date)
	}

	// Currency defaults to USD.
	if args.Currency == "" {
		args.Currency = "USD"
	}
	args.Currency = strings.ToUpper(strings.TrimSpace(args.Currency))

	// --- Save to database ---

	if tctx.store == nil {
		return "error: database not available"
	}

	id, err := tctx.store.SaveExpense(
		args.Amount,
		args.Currency,
		args.Vendor,
		args.Category,
		args.Date,
		args.Note,
		tctx.triggerMsgID,
	)
	if err != nil {
		log.Error("saving expense", "err", err)
		return fmt.Sprintf("error saving expense: %v", err)
	}

	// Save line items if provided.
	itemCount := 0
	for _, item := range args.Items {
		desc := strings.TrimSpace(item.Description)
		if desc == "" {
			continue
		}
		qty := item.Quantity
		if qty < 1 {
			qty = 1
		}
		if err := tctx.store.SaveExpenseItem(id, desc, qty, item.UnitPrice, item.TotalPrice); err != nil {
			log.Error("saving expense item", "err", err, "expense_id", id, "item", desc)
			continue // non-fatal — the parent expense is already saved
		}
		itemCount++
	}

	log.Infof("  scan_receipt: %s%.2f at %s (%s, %s) → ID=%d, %d items",
		args.Currency, args.Amount, args.Vendor, args.Category, args.Date, id, itemCount)

	// Return a confirmation the agent can reference in its reply.
	result := fmt.Sprintf("expense saved ID=%d: %s %.2f at %s (%s, %s)",
		id, args.Currency, args.Amount, args.Vendor, args.Category, args.Date)
	if itemCount > 0 {
		result += fmt.Sprintf(" with %d line items", itemCount)
	}
	return result
}

// execQueryExpenses handles the query_expenses tool call. Returns expense
// data so the agent can answer questions like "what do my finances look like?"
// or "how much did I spend on groceries this month?"
func execQueryExpenses(argsJSON string, tctx *toolContext) string {
	var args struct {
		Period    string `json:"period"`     // "week", "month", "year", "all", or custom range
		Category  string `json:"category"`   // optional filter
		StartDate string `json:"start_date"` // optional YYYY-MM-DD
		EndDate   string `json:"end_date"`   // optional YYYY-MM-DD
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	if tctx.store == nil {
		return "error: database not available"
	}

	// Resolve date range from period shorthand.
	now := time.Now()
	startDate := args.StartDate
	endDate := args.EndDate

	if startDate == "" || endDate == "" {
		switch strings.ToLower(args.Period) {
		case "week":
			// Go back to the start of this week (Monday).
			weekday := int(now.Weekday())
			if weekday == 0 {
				weekday = 7 // Sunday → 7
			}
			startDate = now.AddDate(0, 0, -(weekday - 1)).Format("2006-01-02")
			endDate = now.Format("2006-01-02")
		case "month":
			startDate = time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location()).Format("2006-01-02")
			endDate = now.Format("2006-01-02")
		case "year":
			startDate = time.Date(now.Year(), 1, 1, 0, 0, 0, 0, now.Location()).Format("2006-01-02")
			endDate = now.Format("2006-01-02")
		default: // "all" or empty — show everything
			startDate = "2000-01-01"
			endDate = "2099-12-31"
		}
	}

	// Get summary stats.
	total, byCategory, count, err := tctx.store.ExpenseSummary(startDate, endDate)
	if err != nil {
		log.Error("querying expense summary", "err", err)
		return fmt.Sprintf("error querying expenses: %v", err)
	}

	if count == 0 {
		return fmt.Sprintf("no expenses recorded between %s and %s", startDate, endDate)
	}

	// Build a readable summary for the agent.
	var b strings.Builder
	fmt.Fprintf(&b, "## Expense Summary (%s to %s)\n\n", startDate, endDate)
	fmt.Fprintf(&b, "**Total:** $%.2f across %d transactions\n\n", total, count)

	if len(byCategory) > 0 {
		b.WriteString("**By category:**\n")
		for cat, sum := range byCategory {
			fmt.Fprintf(&b, "- %s: $%.2f\n", cat, sum)
		}
		b.WriteString("\n")
	}

	// Also fetch recent individual expenses for detail.
	expenses, items, err := tctx.store.RecentExpenses(10)
	if err == nil && len(expenses) > 0 {
		b.WriteString("**Recent transactions:**\n")
		for _, e := range expenses {
			fmt.Fprintf(&b, "- %s: %s %.2f at %s (%s)", e.Date, e.Currency, e.Amount, e.Vendor, e.Category)
			if e.Note != "" {
				fmt.Fprintf(&b, " — %s", e.Note)
			}
			b.WriteString("\n")
			// Show line items if available.
			if lineItems, ok := items[e.ID]; ok && len(lineItems) > 0 {
				for _, item := range lineItems {
					fmt.Fprintf(&b, "    • %s", item.Description)
					if item.Quantity > 1 {
						fmt.Fprintf(&b, " x%d", item.Quantity)
					}
					fmt.Fprintf(&b, " — $%.2f\n", item.TotalPrice)
				}
			}
		}
	}

	log.Infof("  query_expenses: %s to %s → $%.2f, %d transactions", startDate, endDate, total, count)

	return b.String()
}
