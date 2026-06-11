// Package tools — shared helper for save_memory and save_self_memory handlers.
//
// Both tools call the same underlying logic with different "subject" values:
// save_memory passes "user", save_self_memory passes "self". This file holds
// the shared ExecSaveMemory function so the two thin wrapper handlers don't
// duplicate logic.
//
// Quality gates (style, length, dedup, classifier, self-safety) are handled
// by the consolidated memgate pipeline — this file just wires inputs and
// handles the post-validation save.
package tools

import (
	"encoding/json"
	"fmt"
	"strings"

	"her/logger"
	"her/tools/memgate"
)

// memoryLog is a logger for memory-saving operations.
var memoryLog = logger.WithPrefix("tools/memory")

// StyleBlocklist returns the style blocklist for callers that need direct
// access (e.g. rejection messages). Delegates to memgate.
func StyleBlocklist() []string {
	return memgate.StyleBlocklist()
}

// MaxMemoryLength returns the maximum allowed memory character count.
func MaxMemoryLength() int {
	return memgate.MaxLength()
}

// ExecSaveMemory is the shared implementation behind save_memory and save_self_memory.
//
// The subject parameter distinguishes the two tools: "user" for save_memory,
// "self" for save_self_memory. Everything else is identical — same quality
// pipeline, same embedding strategy.
func ExecSaveMemory(argsJSON, subject string, ctx *Context) string {
	var args struct {
		CardSlug   string `json:"card_slug"`
		Memory     string `json:"memory"`
		Category   string `json:"category"`
		Tags       string `json:"tags"`
		Importance int    `json:"importance"` // 1-10, defaults to 5 if omitted or out of range
		Context    string `json:"context"`    // optional: why this memory matters
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	// Resolve card_slug to card_id. Every new memory must belong to a card.
	var cardID int64
	if args.CardSlug != "" {
		card, err := ctx.Store.GetCard(args.CardSlug)
		if err != nil {
			return fmt.Sprintf("error looking up card %q: %v", args.CardSlug, err)
		}
		if card == nil {
			return fmt.Sprintf("error: no card found with slug %q. Use list_cards to see available cards, or create_card to make a new one.", args.CardSlug)
		}
		cardID = card.ID
	}

	// Run the consolidated quality pipeline (style → length → dedup → classifier → safety).
	verdict := memgate.RunPipeline(memgate.PipelineInput{
		Text:     args.Memory,
		Subject:  subject,
		Tags:     args.Tags,
		Category: args.Category,
		Context:  args.Context,
	}, memgate.PipelineDeps{
		Store:               ctx.Store,
		EmbedClient:         ctx.EmbedClient,
		ClassifierLLM:       ctx.ClassifierLLM,
		SimilarityThreshold: ctx.SimilarityThreshold,
		MaxLength:           ctx.MaxMemoryLength,
		ConversationID:      ctx.ConversationID,
		TriggerMsgID:        ctx.TriggerMsgID,
		Snippet:             ctx.ClassifierSnippet,
		PreApproved:         ctx.PreApprovedRewrites,
	})

	if verdict.IsSplit && len(verdict.Splits) >= 2 {
		return ExecSplitMemories(verdict.Splits, args.Category, subject, cardID, ctx)
	}
	if !verdict.Allowed {
		return verdict.Reason
	}

	// Reuse embeddings computed by the dedup gate (avoids double-embedding).
	newVec := verdict.TagVec
	textVec := verdict.TextVec

	if ctx.Store == nil {
		return "error: no store configured"
	}

	// Clamp importance to 1-10, defaulting to 5 if omitted or out of range.
	importance := args.Importance
	if importance < 1 || importance > 10 {
		importance = 5
	}

	id, err := ctx.Store.SaveMemory(args.Memory, args.Category, subject, 0, importance, newVec, textVec, args.Tags, args.Context, cardID)
	if err != nil {
		return fmt.Sprintf("error saving memory: %v", err)
	}

	label := "user memory"
	if subject == "self" {
		label = "self memory"
	}

	ctx.SavedMemories = append(ctx.SavedMemories, args.Memory)

	return fmt.Sprintf("saved %s ID=%d: %s", label, id, args.Memory)
}

// ExecSplitMemories saves each sub-memory produced by splitting a compound memory.
//
// Sub-memories skip the classifier — they're either classifier-generated (SPLIT
// verdict) or user-requested (split_memory tool). Each gets its own embedding
// (using the content text directly, since we don't have per-sub-memory tags)
// and an autolink pass.
func ExecSplitMemories(splits []string, category, subject string, cardID int64, ctx *Context) string {
	label := "user memory"
	if subject == "self" {
		label = "self memory"
	}

	var results []string
	for _, content := range splits {
		content = strings.TrimSpace(content)
		if content == "" {
			continue
		}

		// Embed by content text — no per-sub-memory tags provided by classifier.
		// Pass same vec for both tag slot and text slot so dedup works on both.
		var vec []float32
		if ctx.EmbedClient != nil {
			var err error
			vec, err = ctx.EmbedClient.Embed(content)
			if err != nil {
				memoryLog.Warn("split memory: embedding failed", "err", err)
			}
		}

		id, err := ctx.Store.SaveMemory(content, category, subject, 0, 5, vec, vec, "", "", cardID)
		if err != nil {
			memoryLog.Error("split memory: save failed", "err", err)
			continue
		}

		// Autolink so the new sub-memory connects to related existing memories.
		if ctx.EmbedClient != nil && len(vec) > 0 {
			_ = ctx.Store.AutoLinkMemory(id, vec)
		}

		ctx.SavedMemories = append(ctx.SavedMemories, content)
		results = append(results, fmt.Sprintf("%s ID=%d: %s", label, id, content))
		memoryLog.Info("split memory saved", "id", id, "content", content[:min(len(content), 60)])
	}

	if len(results) == 0 {
		return "split failed: no sub-memories could be saved"
	}
	return fmt.Sprintf("split into %d memories:\n%s", len(results), strings.Join(results, "\n"))
}
