// Package merge_memories implements the merge_memories tool — consolidates
// multiple redundant memories into a single, richer memory. Used exclusively
// by the memory dreamer during the nightly dream cycle.
//
// The merge is atomic: save new → deactivate sources → create supersession
// chains → auto-link → audit log. If dry_run is enabled in config, the
// audit is logged but no DB mutations occur.
package merge_memories

import (
	"encoding/json"
	"fmt"
	"strings"

	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/merge_memories")

func init() {
	tools.Register("merge_memories", Handle)
}

// Handle merges multiple memories into one consolidated entry.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		MemoryIDs  []int64 `json:"memory_ids"`
		MergedText string  `json:"merged_text"`
		Category   string  `json:"category"`
		Tags       string  `json:"tags"`
		Reason     string  `json:"reason"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	if len(args.MemoryIDs) < 2 {
		return "error: merge_memories requires at least 2 memory_ids"
	}
	if args.MergedText == "" {
		return "error: merged_text is required"
	}
	if args.Category == "" {
		return "error: category is required"
	}

	// Verify all source memories exist and are active.
	var beforeParts []string
	subject := "user"
	for _, id := range args.MemoryIDs {
		m, err := ctx.Store.GetMemory(id)
		if err != nil || m == nil {
			return fmt.Sprintf("error: memory ID=%d not found", id)
		}
		if !m.Active {
			return fmt.Sprintf("error: memory ID=%d is already inactive", id)
		}
		subject = m.Subject
		beforeParts = append(beforeParts, fmt.Sprintf("[ID=%d] %s", m.ID, m.Content))
	}
	beforeText := strings.Join(beforeParts, " | ")

	// Dry-run: log what would happen but don't touch the DB.
	dryRun := ctx.Cfg != nil && ctx.Cfg.Dream.DryRun
	if dryRun {
		if ctx.Store != nil {
			_ = ctx.Store.SaveDreamAudit("merge", args.MemoryIDs, 0, beforeText, args.MergedText, args.Reason, true)
		}
		log.Infof("  merge_memories [DRY RUN]: would merge %d memories → %q", len(args.MemoryIDs), truncate(args.MergedText, 80))
		return fmt.Sprintf("[DRY RUN] would merge %d memories into: %s (reason: %s)",
			len(args.MemoryIDs), truncate(args.MergedText, 100), args.Reason)
	}

	// Embed the merged text for semantic search.
	var vec []float32
	if ctx.EmbedClient != nil {
		var err error
		vec, err = ctx.EmbedClient.Embed(args.MergedText)
		if err != nil {
			log.Warn("merge_memories: embedding failed", "err", err)
		}
	}

	// Save the consolidated memory.
	newID, err := ctx.Store.SaveMemory(
		args.MergedText, args.Category, subject, 0, 5,
		vec, vec, args.Tags, "",
	)
	if err != nil {
		return fmt.Sprintf("error saving merged memory: %v", err)
	}

	// Deactivate sources and create supersession chains.
	for _, id := range args.MemoryIDs {
		if err := ctx.Store.SupersedeMemory(id, newID, args.Reason); err != nil {
			log.Warn("merge_memories: supersede failed", "source_id", id, "err", err)
		}
	}

	// Auto-link the new memory into the knowledge graph.
	if ctx.EmbedClient != nil && len(vec) > 0 {
		_ = ctx.Store.AutoLinkMemory(newID, vec)
	}

	// Audit log.
	_ = ctx.Store.SaveDreamAudit("merge", args.MemoryIDs, newID, beforeText, args.MergedText, args.Reason, false)

	log.Infof("  merge_memories: merged %v → ID=%d: %s", args.MemoryIDs, newID, truncate(args.MergedText, 80))
	return fmt.Sprintf("merged %d memories → new ID=%d: %s (reason: %s)",
		len(args.MemoryIDs), newID, truncate(args.MergedText, 100), args.Reason)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
