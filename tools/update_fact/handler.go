// Package update_fact implements the update_fact tool — replaces an existing
// fact with a new version when information changes or gets refined.
//
// Instead of overwriting in-place (which destroys history), this creates a
// NEW fact and supersedes the old one. The supersession chain lets the agent
// trace knowledge evolution: "you used to work at X, now at Y."
//
// The new fact gets full SaveFact treatment — embedding, auto-linking,
// classifier gate — for free. The old fact is marked inactive with a
// superseded_by pointer to the new one.
package update_fact

import (
	"encoding/json"
	"fmt"
	"strings"

	"her/classifier"
	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/update_fact")

func init() {
	tools.Register("update_fact", Handle)
}

// Handle creates a new fact that supersedes an existing one. The old fact
// is soft-deleted with a supersession chain pointing to the new version.
// Applies the same style/length/classifier gates as save_fact.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		FactID   int64  `json:"fact_id"`
		Fact     string `json:"fact"`
		Category string `json:"category"`
		Tags     string `json:"tags"`
		Context  string `json:"context"` // optional: why this fact matters
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	// Apply the same style and length gates as save_fact.
	// Trailing em dash check: catches the AI tic of sentences that hang with
	// "—" at the end. Mid-sentence em dashes are fine — only trailing ones blocked.
	trimmed := strings.TrimSpace(args.Fact)
	if strings.HasSuffix(trimmed, "\u2014") || strings.HasSuffix(trimmed, "\u2013") {
		log.Warn("blocked fact update (trailing em dash)", "fact", args.Fact)
		return "rejected: rewrite this fact — it ends with a trailing em dash. Complete the sentence."
	}
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

	// --- Read old fact ---
	// We need the old fact's subject and source_message_id so the new
	// fact inherits them. Also used to show the classifier the delta.
	oldFact, err := ctx.Store.GetFact(args.FactID)
	if err != nil {
		return fmt.Sprintf("error reading old fact: %v", err)
	}
	if oldFact == nil {
		return fmt.Sprintf("fact ID=%d not found", args.FactID)
	}

	// --- Classifier gate ---
	// Show the classifier BOTH old and new text so it can evaluate the
	// delta — without this, it can't tell that an addition was inferred.
	//
	// Pre-approved bypass: if the classifier previously suggested this exact
	// text as a rewrite, skip re-classification.
	if ctx.ClassifierLLM != nil {
		if ctx.PreApprovedRewrites != nil && ctx.PreApprovedRewrites[strings.ToLower(args.Fact)] {
			log.Info("classifier bypass: update matches pre-approved rewrite", "fact", args.Fact)
		} else {
			snippet := ctx.ClassifierSnippet
			if snippet == nil {
				snippet, _ = ctx.Store.RecentMessages(ctx.ConversationID, 1)
			}
			classifyContent := fmt.Sprintf("Original fact: %s\nUpdated fact: %s", oldFact.Fact, args.Fact)
			verdict := classifier.Check(ctx.ClassifierLLM, "fact", classifyContent, snippet)
			_ = ctx.Store.SaveClassifierLog(
				ctx.ConversationID, "fact", verdict.Type, classifyContent, verdict.Reason, verdict.Rewrite,
			)
			if verdict.Rewrite != "" && ctx.PreApprovedRewrites != nil {
				ctx.PreApprovedRewrites[strings.ToLower(verdict.Rewrite)] = true
			}
			if !verdict.Allowed {
				return classifier.RejectionMessage(verdict)
			}
		}
	}

	// --- Embed the new fact ---
	var tagVec, textVec []float32
	if ctx.EmbedClient != nil {
		embedText := args.Tags
		if embedText == "" {
			embedText = args.Fact
		}
		tagVec, _ = ctx.EmbedClient.Embed(embedText)
		if args.Tags != "" {
			factTextForEmbed := args.Fact
			if args.Context != "" {
				factTextForEmbed = args.Fact + " " + args.Context
			}
			textVec, _ = ctx.EmbedClient.Embed(factTextForEmbed)
		}
	}

	// --- Save new fact + supersede old ---
	// SaveFact handles embedding storage, vec_facts index, and auto-linking.
	newID, err := ctx.Store.SaveFact(
		args.Fact, args.Category, oldFact.Subject,
		oldFact.SourceMessageID, 5,
		tagVec, textVec, args.Tags, args.Context,
	)
	if err != nil {
		return fmt.Sprintf("error saving updated fact: %v", err)
	}

	// Mark the old fact as superseded by the new one.
	if err := ctx.Store.SupersedeFact(args.FactID, newID, "updated"); err != nil {
		// The new fact was saved but the chain failed — log but don't fail
		// the whole operation. The new fact is still valid.
		log.Warnf("failed to create supersession chain %d → %d: %v", args.FactID, newID, err)
	}

	log.Infof("  update_fact: ID=%d superseded by ID=%d → %s", args.FactID, newID, args.Fact)
	return fmt.Sprintf("updated: old fact ID=%d superseded by new fact ID=%d: %s", args.FactID, newID, args.Fact)
}
