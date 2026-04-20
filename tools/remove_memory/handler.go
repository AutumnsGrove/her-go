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
	"strings"

	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/remove_memory")

func init() {
	tools.Register("remove_memory", Handle)
}

// Handle soft-deletes one or more memories by ID. Supports both single
// (memory_id) and batch (memory_ids) modes. If replaced_by is provided
// (single mode only), it creates a supersession chain — recording which
// newer memory replaced this one and why.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		MemoryID   int64   `json:"memory_id"`
		MemoryIDs  []int64 `json:"memory_ids"`
		Reason     string  `json:"reason"`
		ReplacedBy int64   `json:"replaced_by"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	// Batch mode: deactivate multiple memories at once.
	if len(args.MemoryIDs) > 0 {
		var removed int
		var errors []string
		for _, id := range args.MemoryIDs {
			if err := ctx.Store.DeactivateMemory(id); err != nil {
				errors = append(errors, fmt.Sprintf("ID=%d: %v", id, err))
				continue
			}
			removed++
		}
		log.Infof("  remove_memory: batch deactivated %d/%d (reason: %s)", removed, len(args.MemoryIDs), args.Reason)
		if len(errors) > 0 {
			return fmt.Sprintf("removed %d/%d memories (reason: %s); errors: %s",
				removed, len(args.MemoryIDs), args.Reason, strings.Join(errors, "; "))
		}
		return fmt.Sprintf("removed %d memories (reason: %s)", removed, args.Reason)
	}

	// Single mode: require memory_id.
	if args.MemoryID == 0 {
		return "error: provide either memory_id (single) or memory_ids (batch)"
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
