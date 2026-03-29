package cmd

import (
	"fmt"
	"strings"

	"her/config"
	"her/memory"

	"github.com/spf13/cobra"
)

var usageCmd = &cobra.Command{
	Use:   "usage",
	Short: "Show API cost and token usage",
	Long: `Displays a breakdown of API costs and token usage across time
periods and models. All data comes from the metrics table in the
local SQLite database.`,
	RunE: runUsage,
}

func init() {
	rootCmd.AddCommand(usageCmd)
}

func runUsage(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	store, err := memory.NewStore(cfg.Memory.DBPath, cfg.Embed.Dimension)
	if err != nil {
		return fmt.Errorf("opening database: %w", err)
	}
	defer store.Close()

	report, err := store.GetUsageReport()
	if err != nil {
		return fmt.Errorf("generating report: %w", err)
	}

	// Header.
	fmt.Println("API Usage")
	fmt.Println(strings.Repeat("─", 50))

	// Period summary table.
	fmt.Println()
	fmt.Printf("  %-16s %8s %12s %10s\n", "Period", "Calls", "Tokens", "Cost")
	fmt.Printf("  %-16s %8s %12s %10s\n", "──────", "─────", "──────", "────")
	for _, p := range report.Periods {
		fmt.Printf("  %-16s %8s %12s %10s\n",
			p.Label,
			formatInt(p.Calls),
			formatInt(p.Tokens),
			formatCost(p.CostUSD),
		)
	}

	// Per-model breakdown.
	if len(report.ByModel) > 0 {
		fmt.Println()
		fmt.Println("By Model")
		fmt.Println(strings.Repeat("─", 50))
		fmt.Println()
		for _, m := range report.ByModel {
			// Shorten model names for readability — strip the provider
			// prefix (e.g. "deepseek/deepseek-chat-v3" → "deepseek-chat-v3").
			name := shortModelName(m.Model)
			fmt.Printf("  %s\n", name)
			fmt.Printf("    calls: %s  prompt: %s  completion: %s  cost: %s\n",
				formatInt(m.Calls),
				formatInt(m.PromptTokens),
				formatInt(m.CompletionTokens),
				formatCost(m.CostUSD),
			)
		}
	}

	fmt.Println()
	return nil
}

// shortModelName strips the "provider/" prefix from OpenRouter model IDs.
// "deepseek/deepseek-chat-v3-0324" becomes "deepseek-chat-v3-0324".
func shortModelName(model string) string {
	if idx := strings.Index(model, "/"); idx >= 0 {
		return model[idx+1:]
	}
	return model
}

// formatInt adds comma separators for readability (e.g. 1234567 → "1,234,567").
func formatInt(n int) string {
	if n == 0 {
		return "0"
	}
	// Work backwards, inserting commas every 3 digits.
	s := fmt.Sprintf("%d", n)
	if len(s) <= 3 {
		return s
	}

	var result []byte
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			result = append(result, ',')
		}
		result = append(result, byte(c))
	}
	return string(result)
}

// formatCost renders a dollar amount. Shows 4 decimal places for small
// amounts (under $1) so you can see sub-cent costs, 2 places otherwise.
func formatCost(usd float64) string {
	if usd == 0 {
		return "$0.00"
	}
	if usd < 1.0 {
		return fmt.Sprintf("$%.4f", usd)
	}
	return fmt.Sprintf("$%.2f", usd)
}
