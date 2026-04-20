// Package split_memory implements the split_memory tool — breaks a compound
// memory into individual facts.
//
// When a memory ends up containing multiple unrelated ideas ("lives in Portland,
// works at Acme, has a dog named Biscuit"), this tool deactivates the original
// and creates separate memories for each fact. The new memories go through the
// same embedding + autolink pipeline as save_memory, but skip the classifier
// (the agent explicitly decided what to split into).
package split_memory

import (
	"encoding/json"
	"fmt"

	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/split_memory")

func init() {
	tools.Register("split_memory", Handle)
}

// Handle splits a compound memory into individual facts. Looks up the original
// memory to inherit its category and subject, deactivates it, then saves each
// new fact through the shared split pipeline (embedding + autolink, no classifier).
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		MemoryID int64    `json:"memory_id"`
		NewFacts []string `json:"new_facts"`
		Reason   string   `json:"reason"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	if len(args.NewFacts) < 2 {
		return "error: need at least 2 new_facts to split into"
	}

	// Look up the original memory to inherit category and subject.
	// GetMemory returns nil (not error) when the ID doesn't exist.
	original, err := ctx.Store.GetMemory(args.MemoryID)
	if err != nil {
		return fmt.Sprintf("error looking up memory #%d: %v", args.MemoryID, err)
	}
	if original == nil {
		return fmt.Sprintf("error: memory #%d not found", args.MemoryID)
	}

	// Deactivate the original — soft-delete so it's preserved for audit.
	if err := ctx.Store.DeactivateMemory(args.MemoryID); err != nil {
		return fmt.Sprintf("error deactivating original memory #%d: %v", args.MemoryID, err)
	}

	log.Infof("  split_memory: deactivated #%d, creating %d new facts (reason: %s)",
		args.MemoryID, len(args.NewFacts), args.Reason)

	// Save each new fact via the shared split pipeline. This handles
	// embedding, autolink, and SavedMemories tracking — same path the
	// classifier's SPLIT verdict uses.
	result := tools.ExecSplitMemories(args.NewFacts, original.Category, original.Subject, ctx)

	return fmt.Sprintf("split #%d (%s): %s", args.MemoryID, args.Reason, result)
}
