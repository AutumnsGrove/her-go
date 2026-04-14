// Package update_expense implements the update_expense tool — modifies fields
// on an existing expense record.
//
// The agent passes only the fields that need changing — all other fields are
// omitted (zero values) and preserved as-is in the database.
package update_expense

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/update_expense")

// validCategories mirrors the set in scan_receipt. Kept local to avoid a
// cross-package dependency between two sibling tool packages.
var validCategories = map[string]bool{
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

var updateDatePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

func init() {
	tools.Register("update_expense", Handle)
}

// Handle updates fields on an existing expense. Only non-zero fields are
// applied — the store method handles the "update only what changed" logic.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		ID       int64   `json:"id"`
		Amount   float64 `json:"amount"`
		Currency string  `json:"currency"`
		Vendor   string  `json:"vendor"`
		Category string  `json:"category"`
		Date     string  `json:"date"`
		Note     string  `json:"note"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	if args.ID <= 0 {
		return "error: expense ID is required"
	}

	if ctx.Store == nil {
		return "error: database not available"
	}

	// Validate category if provided.
	if args.Category != "" {
		args.Category = strings.ToLower(strings.TrimSpace(args.Category))
		if !validCategories[args.Category] {
			return fmt.Sprintf("error: invalid category %q", args.Category)
		}
	}

	// Validate date if provided.
	if args.Date != "" && !updateDatePattern.MatchString(args.Date) {
		return fmt.Sprintf("error: date must be YYYY-MM-DD format, got %q", args.Date)
	}

	// Normalize vendor if provided. OCR output is often mixed-case.
	if args.Vendor != "" {
		raw := strings.TrimSpace(args.Vendor)
		words := strings.Fields(strings.ToLower(raw))
		for i, w := range words {
			if len(w) > 0 {
				words[i] = strings.ToUpper(w[:1]) + w[1:]
			}
		}
		args.Vendor = strings.Join(words, " ")
	}

	if args.Currency != "" {
		args.Currency = strings.ToUpper(strings.TrimSpace(args.Currency))
	}

	err := ctx.Store.UpdateExpense(args.ID, args.Amount, args.Currency, args.Vendor, args.Category, args.Date, args.Note)
	if err != nil {
		log.Error("updating expense", "err", err)
		return fmt.Sprintf("error updating expense: %v", err)
	}

	log.Infof("  update_expense: updated ID=%d", args.ID)
	return fmt.Sprintf("expense ID=%d updated successfully", args.ID)
}
