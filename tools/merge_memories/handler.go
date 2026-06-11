// Package merge_memories implements the merge_memories tool — consolidates
// multiple redundant memories into a single, richer memory. Used exclusively
// by the memory dreamer during the nightly dream cycle.
//
// The merge runs through the memgate quality pipeline (style, length,
// classifier, self-safety). Dedup is skipped because merged text is
// intentionally similar to the sources.
//
// Flow: validate → memgate pipeline → deactivate sources → save →
// supersede chains → auto-link → audit log.
package merge_memories

import (
	"encoding/json"
	"fmt"
	"strings"

	"her/logger"
	"her/tools"
	"her/tools/memgate"
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

	// Run the consolidated quality pipeline (style → length → classifier → safety).
	// Dedup is skipped — merged text is intentionally similar to sources.
	verdict := memgate.RunPipeline(memgate.PipelineInput{
		Text:    args.MergedText,
		Subject: subject,
		Tags:    args.Tags,
	}, memgate.PipelineDeps{
		Store:          ctx.Store,
		ClassifierLLM:  ctx.ClassifierLLM,
		MaxLength:      ctx.MaxMemoryLength,
		ConversationID: ctx.ConversationID,
		TriggerMsgID:   ctx.TriggerMsgID,
		SkipDedup:      true,
	})

	if !verdict.Allowed {
		log.Warn("merge_memories: pipeline rejected", "reason", verdict.Reason)
		_ = ctx.Store.SaveDreamAudit("merge", args.MemoryIDs, 0, beforeText, args.MergedText,
			fmt.Sprintf("REJECTED: %s", verdict.Reason), false)
		return fmt.Sprintf("rejected: %s", verdict.Reason)
	}

	// --- Deactivate sources AFTER validation passes ---
	// Sources are soft-deleted only after the pipeline confirms the merged
	// text is acceptable. This prevents the old bug where rejected merges
	// left source memories orphaned.
	for _, id := range args.MemoryIDs {
		if err := ctx.Store.DeactivateMemory(id); err != nil {
			log.Warn("merge_memories: deactivate failed", "source_id", id, "err", err)
		}
	}

	// --- Embed the merged text ---
	var vec []float32
	if ctx.EmbedClient != nil {
		var err error
		embedText := args.Tags
		if embedText == "" {
			embedText = args.MergedText
		}
		vec, err = ctx.EmbedClient.Embed(embedText)
		if err != nil {
			log.Warn("merge_memories: embedding failed", "err", err)
		}
	}

	// --- Save the consolidated memory ---
	newID, err := ctx.Store.SaveMemory(
		args.MergedText, args.Category, subject, 0, 5,
		vec, vec, args.Tags, "", 0,
	)
	if err != nil {
		return fmt.Sprintf("error saving merged memory: %v", err)
	}

	// Create supersession chains (source → new).
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
