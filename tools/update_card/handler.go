// Package update_card implements the update_card tool — rewrites a memory
// card's content to incorporate new information.
//
// The content goes through the same classifier quality gates as the old
// save_memory flow. The delta is logged to the memory_log table.
package update_card

import (
	"encoding/json"
	"fmt"

	"her/classifier"
	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/update_card")

func init() {
	tools.Register("update_card", Handle)
}

type args struct {
	TopicSlug string `json:"topic_slug"`
	Content   string `json:"content"`
	Delta     string `json:"delta"`
}

// Handle rewrites a card's content and logs the change.
func Handle(argsJSON string, ctx *tools.Context) string {
	var a args
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "error: invalid arguments"
	}
	if a.TopicSlug == "" {
		return "error: topic_slug is required"
	}
	if a.Content == "" {
		return "error: content is required"
	}
	if a.Delta == "" {
		return "error: delta is required"
	}

	// Check the card exists before updating.
	card, err := ctx.Store.GetCard(a.TopicSlug)
	if err != nil {
		log.Error("update_card lookup failed", "err", err)
		return fmt.Sprintf("error: %v", err)
	}
	if card == nil {
		return fmt.Sprintf("error: no card found with slug %q", a.TopicSlug)
	}

	// Run classifier gate if available. Fail-open if ClassifierLLM is nil.
	if ctx.ClassifierLLM != nil {
		writeType := "memory"
		if card.Subject == "self" {
			writeType = "self_memory"
		}

		snippet := ctx.ClassifierSnippet
		if snippet == nil {
			snippet, _ = ctx.Store.RecentMessages(ctx.ConversationID, 1)
		}

		verdict := classifier.Check(ctx.ClassifierLLM, writeType, a.Content, snippet)
		_ = ctx.Store.SaveClassifierLog(
			ctx.ConversationID, writeType, verdict.Type, a.Content, verdict.Reason, verdict.Rewrite,
		)

		if !verdict.Allowed {
			log.Infof("update_card rejected: %s — %s", verdict.Type, verdict.Reason)
			return classifier.RejectionMessage(verdict)
		}

		// Self-memory safety gate — catches feedback-loop patterns.
		if card.Subject == "self" {
			safetyVerdict := classifier.Check(ctx.ClassifierLLM, "self_memory_safety", a.Content, snippet)
			_ = ctx.Store.SaveClassifierLog(
				ctx.ConversationID, "self_memory_safety", safetyVerdict.Type, a.Content, safetyVerdict.Reason, "",
			)
			if !safetyVerdict.Allowed {
				log.Warn("self-memory safety gate rejected", "verdict", safetyVerdict.Type, "reason", safetyVerdict.Reason)
				return classifier.RejectionMessage(safetyVerdict)
			}
		}
	}

	updated, err := ctx.Store.UpdateCard(a.TopicSlug, a.Content, a.Delta, 0)
	if err != nil {
		log.Error("update_card failed", "err", err)
		return fmt.Sprintf("error: %v", err)
	}

	log.Infof("updated card %s (v%d): %s", a.TopicSlug, updated.Version, a.Delta)
	return fmt.Sprintf("Updated card %q (v%d). Delta: %s", a.TopicSlug, updated.Version, a.Delta)
}
