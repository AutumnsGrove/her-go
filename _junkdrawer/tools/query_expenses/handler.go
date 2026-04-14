// Package query_expenses implements the query_expenses tool — retrieves
// expense history for a given time period.
//
// Returns totals, category breakdowns, and recent individual transactions
// so the agent can answer questions like "how much did I spend this month?"
package query_expenses

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/query_expenses")

func init() {
	tools.Register("query_expenses", Handle)
}

// Handle queries expense history for a given period and returns a formatted
// summary. Supports shorthand periods ("week", "month", "year", "all") or
// explicit date ranges via start_date/end_date.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		Period    string `json:"period"`     // "week", "month", "year", "all", or custom range
		Category  string `json:"category"`   // optional filter
		StartDate string `json:"start_date"` // optional YYYY-MM-DD
		EndDate   string `json:"end_date"`   // optional YYYY-MM-DD
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	if ctx.Store == nil {
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
			// time.Weekday() returns 0 for Sunday, 1 for Monday, etc.
			weekday := int(now.Weekday())
			if weekday == 0 {
				weekday = 7 // Sunday → treat as day 7 to keep Monday as start
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
	total, byCategory, count, err := ctx.Store.ExpenseSummary(startDate, endDate)
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
	expenses, items, err := ctx.Store.RecentExpenses(10)
	if err == nil && len(expenses) > 0 {
		b.WriteString("**Recent transactions:**\n")
		for _, e := range expenses {
			fmt.Fprintf(&b, "- [ID=%d] %s: %s %.2f at %s (%s)", e.ID, e.Date, e.Currency, e.Amount, e.Vendor, e.Category)
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
