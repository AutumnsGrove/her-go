// Package remove_memory implements the remove_memory tool — soft-deletes a memory
// that is no longer true, was incorrect, or is redundant with another memory.
//
// Memories are soft-deleted (DeactivateMemory sets active=false), not permanently
// removed. This preserves audit history — we can always see what was learned
// and then corrected.
package remove_memory

import (
	"encoding/json"
	"fmt"

	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/remove_memory")

func init() {
	tools.Register("remove_memory", Handle)
}

// Handle soft-deletes a memory by ID. If replaced_by is provided, it creates a
// supersession chain — recording which newer memory replaced this one and why.
// This preserves knowledge evolution: "used to work at X" → "now at Y."
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		MemoryID   int64  `json:"memory_id"`
		Reason     string `json:"reason"`
		ReplacedBy int64  `json:"replaced_by"` // optional — if set, creates supersession chain
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	// If replaced_by is set, use SupersedeMemory to record the chain.
	// Otherwise, plain DeactivateMemory (same behavior as before).
	if args.ReplacedBy > 0 {
		if err := ctx.Store.SupersedeMemory(args.MemoryID, args.ReplacedBy, args.Reason); err != nil {
			return fmt.Sprintf("error superseding memory: %v", err)
		}
		log.Infof("  remove_memory: superseded ID=%d → ID=%d (reason: %s)", args.MemoryID, args.ReplacedBy, args.Reason)
		return fmt.Sprintf("superseded memory ID=%d → ID=%d (reason: %s)", args.MemoryID, args.ReplacedBy, args.Reason)
	}

	if err := ctx.Store.DeactivateMemory(args.MemoryID); err != nil {
		return fmt.Sprintf("error removing memory: %v", err)
	}

	log.Infof("  remove_memory: deactivated ID=%d (reason: %s)", args.MemoryID, args.Reason)
	return fmt.Sprintf("removed memory ID=%d (reason: %s)", args.MemoryID, args.Reason)
}
