// Package update_memory implements the update_memory tool — replaces an existing
// memory with a new version when information changes or gets refined.
//
// Instead of overwriting in-place (which destroys history), this creates a
// NEW memory and supersedes the old one. The supersession chain lets the agent
// trace knowledge evolution: "you used to work at X, now at Y."
//
// Quality gates (style, length, classifier) are handled by the memgate
// pipeline. Dedup is skipped because the new text intentionally replaces
// the old — similarity is expected.
package update_memory

import (
	"encoding/json"
	"fmt"

	"her/logger"
	"her/tools"
	"her/tools/memgate"
)

var log = logger.WithPrefix("tools/update_memory")

func init() {
	tools.Register("update_memory", Handle)
}

// Handle creates a new memory that supersedes an existing one. The old memory
// is soft-deleted with a supersession chain pointing to the new version.
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

	// SelfOnly guard — the introspection agent should only modify self-memories.
	if ctx.SelfOnly && oldMemory.Subject != "self" {
		return fmt.Sprintf("rejected: memory ID=%d is a %s memory, not a self-memory", args.MemoryID, oldMemory.Subject)
	}

	// Run the consolidated quality pipeline (style → length → classifier).
	// Dedup is skipped — updates intentionally produce similar text.
	// OldText shows the classifier both versions for delta evaluation.
	verdict := memgate.RunPipeline(memgate.PipelineInput{
		Text:    args.Memory,
		Subject: oldMemory.Subject,
		Tags:    args.Tags,
		Context: args.Context,
		OldText: oldMemory.Content,
	}, memgate.PipelineDeps{
		Store:          ctx.Store,
		EmbedClient:    ctx.EmbedClient,
		ClassifierLLM:  ctx.ClassifierLLM,
		MaxLength:      ctx.MaxMemoryLength,
		ConversationID: ctx.ConversationID,
		TriggerMsgID:   ctx.TriggerMsgID,
		Snippet:        ctx.ClassifierSnippet,
		PreApproved:    ctx.PreApprovedRewrites,
		SkipDedup:      true,
	})

	if !verdict.Allowed {
		return verdict.Reason
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
	newID, err := ctx.Store.SaveMemory(
		args.Memory, args.Category, oldMemory.Subject,
		oldMemory.SourceMessageID, 5,
		tagVec, textVec, args.Tags, args.Context, 0,
	)
	if err != nil {
		return fmt.Sprintf("error saving updated memory: %v", err)
	}

	// Mark the old memory as superseded by the new one.
	if err := ctx.Store.SupersedeMemory(args.MemoryID, newID, "updated"); err != nil {
		log.Warnf("failed to create supersession chain %d → %d: %v", args.MemoryID, newID, err)
	}

	log.Infof("  update_memory: ID=%d superseded by ID=%d → %s", args.MemoryID, newID, args.Memory)
	return fmt.Sprintf("updated: old memory ID=%d superseded by new memory ID=%d: %s", args.MemoryID, newID, args.Memory)
}
