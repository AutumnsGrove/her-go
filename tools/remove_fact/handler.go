// Package remove_fact implements the remove_fact tool — soft-deletes a fact
// that is no longer true, was incorrect, or is redundant with another fact.
//
// Facts are soft-deleted (DeactivateFact sets active=false), not permanently
// removed. This preserves audit history — we can always see what was learned
// and then corrected.
package remove_fact

import (
	"encoding/json"
	"fmt"

	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/remove_fact")

func init() {
	tools.Register("remove_fact", Handle)
}

// Handle soft-deletes a fact by ID. The reason parameter is for logging only —
// it's not stored in the DB, just surfaced in the return message to help the
// agent confirm what it did.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		FactID int64  `json:"fact_id"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	if err := ctx.Store.DeactivateFact(args.FactID); err != nil {
		return fmt.Sprintf("error removing fact: %v", err)
	}

	log.Infof("  remove_fact: deactivated ID=%d (reason: %s)", args.FactID, args.Reason)
	return fmt.Sprintf("removed fact ID=%d (reason: %s)", args.FactID, args.Reason)
}
