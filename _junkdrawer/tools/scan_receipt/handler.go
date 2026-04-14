// Package scan_receipt implements the scan_receipt tool — saves a purchase
// or expense from a receipt photo or manual mention.
//
// The agent provides structured data parsed from OCR text or from the user
// mentioning spending money. This handler validates, normalizes, and saves
// the expense to the expenses/expense_items tables.
//
// Financial data stays OUT of the facts table — only high-level patterns
// ("user is budgeting carefully") belong there. Individual transactions
// go in the dedicated expense tables.
package scan_receipt

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"time"

	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/scan_receipt")

// validExpenseCategories is the fixed set of categories the agent can assign.
// Using a map[string]bool for O(1) membership check — Go's idiomatic Set.
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

// datePattern validates YYYY-MM-DD format. Compiled once at package init —
// same pattern as the envVarPattern in config.go.
var datePattern = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// lineItem is the JSON structure for individual receipt items.
type lineItem struct {
	Description string  `json:"description"`
	Quantity    int     `json:"quantity"`
	UnitPrice   float64 `json:"unit_price"`
	TotalPrice  float64 `json:"total_price"`
}

// normalizeVendor cleans up vendor names from OCR output. OCR often produces
// mixed casing like "cIDer ceLLar" — we title-case each word for readability.
func normalizeVendor(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	words := strings.Fields(strings.ToLower(raw))
	for i, w := range words {
		if len(w) > 0 {
			// Capitalize the first byte. Works for ASCII vendor names; for
			// full Unicode vendor names the x/text package would be needed.
			words[i] = strings.ToUpper(w[:1]) + w[1:]
		}
	}
	return strings.Join(words, " ")
}

func init() {
	tools.Register("scan_receipt", Handle)
}

// Handle validates and saves an expense from a receipt scan.
//
// Parameters (from agent):
//   - amount:   float, total amount (required)
//   - vendor:   string, store/restaurant name (required)
//   - category: enum, from preset list (required)
//   - date:     string, YYYY-MM-DD (required)
//   - currency: string, ISO 4217 code (optional, defaults to "USD")
//   - note:     string, optional context about the purchase
//   - items:    array of {description, quantity, unit_price, total_price}
func Handle(argsJSON string, ctx *tools.Context) string {
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

	if args.Amount <= 0 {
		return "error: amount must be greater than 0"
	}

	args.Vendor = normalizeVendor(args.Vendor)
	if args.Vendor == "" {
		return "error: vendor name is required"
	}

	args.Category = strings.ToLower(strings.TrimSpace(args.Category))
	if !validExpenseCategories[args.Category] {
		valid := make([]string, 0, len(validExpenseCategories))
		for k := range validExpenseCategories {
			valid = append(valid, k)
		}
		return fmt.Sprintf("error: invalid category %q — valid options: %s", args.Category, strings.Join(valid, ", "))
	}

	args.Date = strings.TrimSpace(args.Date)
	if args.Date == "" {
		args.Date = time.Now().Format("2006-01-02")
	} else if !datePattern.MatchString(args.Date) {
		return fmt.Sprintf("error: date must be YYYY-MM-DD format, got %q", args.Date)
	}

	if args.Currency == "" {
		args.Currency = "USD"
	}
	args.Currency = strings.ToUpper(strings.TrimSpace(args.Currency))

	// --- Classifier gate ---
	// Check if this is a real purchase or an in-game/fictional transaction.
	if ctx.ClassifierLLM != nil && ctx.ClassifyWriteFunc != nil {
		receiptSummary := fmt.Sprintf("%s %.2f at %s (%s)", args.Currency, args.Amount, args.Vendor, args.Category)
		snippet, _ := ctx.Store.RecentMessages(ctx.ConversationID, 3)
		verdict := ctx.ClassifyWriteFunc("receipt", receiptSummary, snippet)
		if !verdict.Allowed {
			if ctx.RejectionMessageFunc != nil {
				return ctx.RejectionMessageFunc(verdict)
			}
			return fmt.Sprintf("rejected by classifier: %s", verdict.Reason)
		}
	}

	// --- Save to database ---

	if ctx.Store == nil {
		return "error: database not available"
	}

	id, err := ctx.Store.SaveExpense(
		args.Amount,
		args.Currency,
		args.Vendor,
		args.Category,
		args.Date,
		args.Note,
		ctx.TriggerMsgID,
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
		if err := ctx.Store.SaveExpenseItem(id, desc, qty, item.UnitPrice, item.TotalPrice); err != nil {
			log.Error("saving expense item", "err", err, "expense_id", id, "item", desc)
			continue // non-fatal — the parent expense is already saved
		}
		itemCount++
	}

	log.Infof("  scan_receipt: %s%.2f at %s (%s, %s) → ID=%d, %d items",
		args.Currency, args.Amount, args.Vendor, args.Category, args.Date, id, itemCount)

	// Build a detailed result string with all the data.
	var result strings.Builder
	fmt.Fprintf(&result, "expense saved ID=%d: %s %.2f at %s (%s, %s)",
		id, args.Currency, args.Amount, args.Vendor, args.Category, args.Date)
	if itemCount > 0 {
		result.WriteString("\nItems:")
		for _, item := range args.Items {
			desc := strings.TrimSpace(item.Description)
			if desc == "" {
				continue
			}
			fmt.Fprintf(&result, "\n  • %s: %s %.2f", desc, args.Currency, item.TotalPrice)
		}
	}

	// Inject into ExpenseContext so the CHAT MODEL sees the exact receipt
	// data in its system prompt. Without this, only the agent model sees
	// the result — the chat model hallucinated vendor/amount details.
	ctx.ExpenseContext = fmt.Sprintf(
		"# Recent Receipt Scan\n\n"+
			"You just scanned a receipt. Use ONLY these exact details in your reply.\n"+
			"IMPORTANT: Item names from receipts are often abbreviated or coded (e.g., 'CHIO BANANAS' means bananas, "+
			"'APL HNYCRISP' means honeycrisp apples). You may lightly interpret obvious abbreviations but do NOT "+
			"invent items that aren't listed here. If an item name is unclear, use it as-is.\n\n%s",
		result.String(),
	)

	return result.String()
}
