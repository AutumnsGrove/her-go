// Package update_memory implements the update_memory tool — replaces an existing
// memory with a new version when information changes or gets refined.
//
// Instead of overwriting in-place (which destroys history), this creates a
// NEW memory and supersedes the old one. The supersession chain lets the agent
// trace knowledge evolution: "you used to work at X, now at Y."
//
// The new memory gets full SaveMemory treatment — embedding, auto-linking,
// classifier gate — for free. The old memory is marked inactive with a
// superseded_by pointer to the new one.
package update_memory

import (
	"encoding/json"
	"fmt"
	"strings"

	"her/classifier"
	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/update_memory")

func init() {
	tools.Register("update_memory", Handle)
}

// Handle creates a new memory that supersedes an existing one. The old memory
// is soft-deleted with a supersession chain pointing to the new version.
// Applies the same style/length/classifier gates as save_memory.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		MemoryID int64  `json:"memory_id"`
		Memory   string `json:"memory"`
		Category string `json:"category"`
		Tags     string `json:"tags"`
		Context  string `json:"context"` // optional: why this memory matters
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	// Apply the same style and length gates as save_memory.
	// Trailing em dash check: catches the AI tic of sentences that hang with
	// "—" at the end. Mid-sentence em dashes are fine — only trailing ones blocked.
	trimmed := strings.TrimSpace(args.Memory)
	if strings.HasSuffix(trimmed, "\u2014") || strings.HasSuffix(trimmed, "\u2013") {
		log.Warn("blocked memory update (trailing em dash)", "memory", args.Memory)
		return "rejected: rewrite this memory — it ends with a trailing em dash. Complete the sentence."
	}
	lower := strings.ToLower(args.Memory)
	for _, blocked := range tools.StyleBlocklist() {
		if strings.Contains(lower, blocked) {
			log.Warn("blocked memory update (style)", "pattern", blocked, "memory", args.Memory)
			return fmt.Sprintf("rejected: rewrite in plain, concise language. Blocked pattern: %q", blocked)
		}
	}
	if len(args.Memory) > tools.MaxMemoryLength() {
		log.Warn("blocked memory update (too long)", "len", len(args.Memory))
		return fmt.Sprintf("rejected: memory is %d characters (max %d). Condense to 1-2 short sentences.", len(args.Memory), tools.MaxMemoryLength())
	}

	// --- Read old memory ---
	// We need the old memory's subject and source_message_id so the new
	// memory inherits them. Also used to show the classifier the delta.
	oldMemory, err := ctx.Store.GetMemory(args.MemoryID)
	if err != nil {
		return fmt.Sprintf("error reading old memory: %v", err)
	}
	if oldMemory == nil {
		return fmt.Sprintf("memory ID=%d not found", args.MemoryID)
	}

	// --- Classifier gate ---
	// Show the classifier BOTH old and new text so it can evaluate the
	// delta — without this, it can't tell that an addition was inferred.
	//
	// Pre-approved bypass: if the classifier previously suggested this exact
	// text as a rewrite, skip re-classification.
	if ctx.ClassifierLLM != nil {
		if ctx.PreApprovedRewrites != nil && ctx.PreApprovedRewrites[strings.ToLower(args.Memory)] {
			log.Info("classifier bypass: update matches pre-approved rewrite", "memory", args.Memory)
		} else {
			snippet := ctx.ClassifierSnippet
			if snippet == nil {
				snippet, _ = ctx.Store.RecentMessages(ctx.ConversationID, 1)
			}
			classifyContent := fmt.Sprintf("Original memory: %s\nUpdated memory: %s", oldMemory.Content, args.Memory)
			verdict := classifier.Check(ctx.ClassifierLLM, "memory", classifyContent, snippet)
			_ = ctx.Store.SaveClassifierLog(
				ctx.ConversationID, "memory", verdict.Type, classifyContent, verdict.Reason, verdict.Rewrite,
			)
			if verdict.Rewrite != "" && ctx.PreApprovedRewrites != nil {
				ctx.PreApprovedRewrites[strings.ToLower(verdict.Rewrite)] = true
			}
			if !verdict.Allowed {
				return classifier.RejectionMessage(verdict)
			}
		}
	}

	// --- Embed the new memory ---
	var tagVec, textVec []float32
	if ctx.EmbedClient != nil {
		embedText := args.Tags
		if embedText == "" {
			embedText = args.Memory
		}
		tagVec, _ = ctx.EmbedClient.Embed(embedText)
		if args.Tags != "" {
			memTextForEmbed := args.Memory
			if args.Context != "" {
				memTextForEmbed = args.Memory + " " + args.Context
			}
			textVec, _ = ctx.EmbedClient.Embed(memTextForEmbed)
		}
	}

	// --- Save new memory + supersede old ---
	// SaveMemory handles embedding storage, vec_memories index, and auto-linking.
	newID, err := ctx.Store.SaveMemory(
		args.Memory, args.Category, oldMemory.Subject,
		oldMemory.SourceMessageID, 5,
		tagVec, textVec, args.Tags, args.Context,
	)
	if err != nil {
		return fmt.Sprintf("error saving updated memory: %v", err)
	}

	// Mark the old memory as superseded by the new one.
	if err := ctx.Store.SupersedeMemory(args.MemoryID, newID, "updated"); err != nil {
		// The new memory was saved but the chain failed — log but don't fail
		// the whole operation. The new memory is still valid.
		log.Warnf("failed to create supersession chain %d → %d: %v", args.MemoryID, newID, err)
	}

	log.Infof("  update_memory: ID=%d superseded by ID=%d → %s", args.MemoryID, newID, args.Memory)
	return fmt.Sprintf("updated: old memory ID=%d superseded by new memory ID=%d: %s", args.MemoryID, newID, args.Memory)
}
