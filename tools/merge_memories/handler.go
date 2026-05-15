// Package merge_memories implements the merge_memories tool — consolidates
// multiple redundant memories into a single, richer memory. Used exclusively
// by the memory dreamer during the nightly dream cycle.
//
// The merge runs through the same quality gates as normal memory saves:
// style blocklist, length limit, and classifier (LOW_VALUE check). Dedup
// is skipped because merged text is intentionally similar to the sources.
//
// Flow: validate → style gate → length gate → deactivate sources →
// classifier → save → supersede chains → auto-link → audit log.
package merge_memories

import (
	"encoding/json"
	"fmt"
	"strings"

	"her/classifier"
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

	// --- Style gate (same blocklist as normal memory saves) ---
	trimmed := strings.TrimSpace(args.MergedText)
	if strings.HasSuffix(trimmed, "—") || strings.HasSuffix(trimmed, "–") {
		return "rejected: merged text ends with a trailing em dash. Complete the sentence and retry."
	}
	lower := strings.ToLower(args.MergedText)
	for _, blocked := range tools.StyleBlocklist() {
		if strings.Contains(lower, blocked) {
			return fmt.Sprintf("rejected: merged text contains AI writing tic %q. Rewrite in plain, concise language and retry.", blocked)
		}
	}

	// --- Length gate ---
	limit := ctx.MaxMemoryLength
	if limit <= 0 {
		limit = tools.MaxMemoryLength()
	}
	if len(args.MergedText) > limit {
		return fmt.Sprintf("rejected: merged text is %d characters (max %d). Condense and retry.", len(args.MergedText), limit)
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

	// --- Deactivate sources BEFORE classifier/save ---
	// This ensures the dedup check (if it ever runs) won't see the originals.
	// Sources are soft-deleted and recoverable if something fails downstream.
	for _, id := range args.MemoryIDs {
		if err := ctx.Store.DeactivateMemory(id); err != nil {
			log.Warn("merge_memories: deactivate failed", "source_id", id, "err", err)
		}
	}

	// --- Classifier gate (LOW_VALUE check) ---
	// Merged content was already approved on initial save, but consolidation
	// could produce something too vague. Skip for self-memories that need the
	// self_memory classifier instead.
	if ctx.ClassifierLLM != nil {
		writeType := "memory"
		if subject == "self" {
			writeType = "self_memory"
		}
		verdict := classifier.Check(ctx.ClassifierLLM, writeType, args.MergedText, nil)
		if !verdict.Allowed {
			// Re-activate sources since we're rejecting the merge.
			// Note: DeactivateMemory is a soft-delete, but there's no
			// ReactivateMemory. We log the rejection — the dreamer can retry.
			log.Warn("merge_memories: classifier rejected", "verdict", verdict.Type, "reason", verdict.Reason)
			_ = ctx.Store.SaveDreamAudit("merge", args.MemoryIDs, 0, beforeText, args.MergedText,
				fmt.Sprintf("REJECTED by classifier: %s — %s", verdict.Type, verdict.Reason), false)
			return fmt.Sprintf("rejected by classifier: %s. Rewrite the merged text and retry.", classifier.RejectionMessage(verdict))
		}

		// Self-memory safety gate (catches sycophancy loops in merged self-observations).
		if subject == "self" {
			safetyVerdict := classifier.Check(ctx.ClassifierLLM, "self_memory_safety", args.MergedText, nil)
			if !safetyVerdict.Allowed {
				log.Warn("merge_memories: self-memory safety rejected", "verdict", safetyVerdict.Type)
				return fmt.Sprintf("rejected: %s. Do not merge self-memories that encode agreement as effective strategy.", safetyVerdict.Reason)
			}
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
