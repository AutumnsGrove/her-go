// Package update_fact implements the update_fact tool — updates an existing
// fact when new information changes or refines what the bot knew.
//
// Applies the same style and length quality gates as save_fact, plus
// re-embeds the updated fact text so semantic search stays accurate.
package update_fact

import (
	"encoding/json"
	"fmt"
	"strings"

	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/update_fact")

func init() {
	tools.Register("update_fact", Handle)
}

// Handle updates an existing fact by ID. Applies style/length gates and
// re-embeds the fact for accurate semantic search. Same gates as save_fact —
// updates shouldn't sneak in AI-slop or paragraphs either.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		FactID     int64  `json:"fact_id"`
		Fact       string `json:"fact"`
		Category   string `json:"category"`
		Importance int    `json:"importance"`
		Tags       string `json:"tags"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	// Clamp importance to [1, 10].
	if args.Importance < 1 {
		args.Importance = 1
	}
	if args.Importance > 10 {
		args.Importance = 10
	}

	// Apply the same style and length gates as save_fact. Updates shouldn't
	// sneak in AI-slop or paragraphs either.
	lower := strings.ToLower(args.Fact)
	for _, blocked := range tools.StyleBlocklist() {
		if strings.Contains(lower, blocked) {
			log.Warn("blocked fact update (style)", "pattern", blocked, "fact", args.Fact)
			return fmt.Sprintf("rejected: rewrite in plain, concise language. Blocked pattern: %q", blocked)
		}
	}
	if len(args.Fact) > tools.MaxFactLength() {
		log.Warn("blocked fact update (too long)", "len", len(args.Fact))
		return fmt.Sprintf("rejected: fact is %d characters (max %d). Condense to 1-2 short sentences.", len(args.Fact), tools.MaxFactLength())
	}

	// --- Classifier gate ---
	if ctx.ClassifierLLM != nil && ctx.ClassifyWriteFunc != nil {
		snippet, _ := ctx.Store.RecentMessages(ctx.ConversationID, 3)
		verdict := ctx.ClassifyWriteFunc("fact", args.Fact, snippet)
		if !verdict.Allowed {
			if ctx.RejectionMessageFunc != nil {
				return ctx.RejectionMessageFunc(verdict)
			}
			return fmt.Sprintf("rejected by classifier: %s", verdict.Reason)
		}
	}

	if err := ctx.Store.UpdateFact(args.FactID, args.Fact, args.Category, args.Importance, args.Tags); err != nil {
		return fmt.Sprintf("error updating fact: %v", err)
	}

	// Re-embed using tags (same as save_fact — embed by topic, not by text).
	// Also re-embed the raw fact text so the cached text embedding stays fresh.
	if ctx.EmbedClient != nil {
		embedText := args.Tags
		if embedText == "" {
			embedText = args.Fact
		}
		if newVec, err := ctx.EmbedClient.Embed(embedText); err == nil {
			// Recompute text embedding. When tags are empty, newVec already
			// encodes the text, so we pass nil to avoid a redundant embed call.
			var newTextVec []float32
			if args.Tags != "" {
				newTextVec, _ = ctx.EmbedClient.Embed(args.Fact)
			}
			_ = ctx.Store.UpdateFactEmbedding(args.FactID, newVec, newTextVec)
			log.Debug("recomputed embedding for updated fact", "fact_id", args.FactID)
		}
	}

	log.Infof("  update_fact: ID=%d → %s", args.FactID, args.Fact)
	return fmt.Sprintf("updated fact ID=%d: %s", args.FactID, args.Fact)
}
