package layers

// Layer 5.5: Expense context.
// When a receipt was just scanned, inject the exact data so the chat
// model references real numbers and vendor names instead of hallucinating.
// Only present for the turn immediately after a scan_receipt tool call.

import "fmt"

func init() {
	Register(PromptLayer{
		Name:    "Expense Context",
		Order:   550,
		Stream:  StreamChat,
		Builder: buildChatExpenses,
	})
}

func buildChatExpenses(ctx *LayerContext) LayerResult {
	if ctx.ExpenseContext == "" {
		return LayerResult{}
	}
	return LayerResult{
		Content: ctx.ExpenseContext,
		Detail:  fmt.Sprintf("%d chars", len(ctx.ExpenseContext)),
	}
}
