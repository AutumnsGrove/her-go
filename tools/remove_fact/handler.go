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

// Handle soft-deletes a fact by ID. If replaced_by is provided, it creates a
// supersession chain — recording which newer fact replaced this one and why.
// This preserves knowledge evolution: "used to work at X" → "now at Y."
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		FactID     int64  `json:"fact_id"`
		Reason     string `json:"reason"`
		ReplacedBy int64  `json:"replaced_by"` // optional — if set, creates supersession chain
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	// If replaced_by is set, use SupersedeFact to record the chain.
	// Otherwise, plain DeactivateFact (same behavior as before).
	if args.ReplacedBy > 0 {
		if err := ctx.Store.SupersedeFact(args.FactID, args.ReplacedBy, args.Reason); err != nil {
			return fmt.Sprintf("error superseding fact: %v", err)
		}
		log.Infof("  remove_fact: superseded ID=%d → ID=%d (reason: %s)", args.FactID, args.ReplacedBy, args.Reason)
		return fmt.Sprintf("superseded fact ID=%d → ID=%d (reason: %s)", args.FactID, args.ReplacedBy, args.Reason)
	}

	if err := ctx.Store.DeactivateFact(args.FactID); err != nil {
		return fmt.Sprintf("error removing fact: %v", err)
	}

	log.Infof("  remove_fact: deactivated ID=%d (reason: %s)", args.FactID, args.Reason)
	return fmt.Sprintf("removed fact ID=%d (reason: %s)", args.FactID, args.Reason)
}
