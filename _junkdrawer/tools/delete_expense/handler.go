// Package delete_expense implements the delete_expense tool — removes an
// expense and its line items by ID.
//
// Used when the user wants to clear test data or correct a mistaken entry.
// Note: in practice the agent should route destructive deletions through
// reply_confirm first, so the user can confirm before data is lost.
package delete_expense

import (
	"encoding/json"
	"fmt"

	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/delete_expense")

func init() {
	tools.Register("delete_expense", Handle)
}

// Handle deletes an expense and all its associated line items by ID.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		ID int64 `json:"id"`
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

	err := ctx.Store.DeleteExpense(args.ID)
	if err != nil {
		log.Error("deleting expense", "err", err)
		return fmt.Sprintf("error deleting expense: %v", err)
	}

	log.Infof("  delete_expense: removed ID=%d", args.ID)
	return fmt.Sprintf("expense ID=%d deleted (including any line items)", args.ID)
}
