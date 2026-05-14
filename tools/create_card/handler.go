// Package create_card implements the create_card tool — creates a new
// organic (unprotected) memory topic card.
package create_card

import (
	"encoding/json"
	"fmt"

	"her/classifier"
	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/create_card")

func init() {
	tools.Register("create_card", Handle)
}

type args struct {
	TopicSlug string `json:"topic_slug"`
	Name      string `json:"name"`
	Content   string `json:"content"`
	Subject   string `json:"subject"`
}

// Handle creates a new memory card.
func Handle(argsJSON string, ctx *tools.Context) string {
	var a args
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "error: invalid arguments"
	}
	if a.TopicSlug == "" || a.Name == "" || a.Content == "" || a.Subject == "" {
		return "error: topic_slug, name, content, and subject are all required"
	}
	if a.Subject != "user" && a.Subject != "self" {
		return "error: subject must be 'user' or 'self'"
	}

	// Check for slug collision.
	existing, err := ctx.Store.GetCard(a.TopicSlug)
	if err != nil {
		log.Error("create_card lookup failed", "err", err)
		return fmt.Sprintf("error: %v", err)
	}
	if existing != nil {
		return fmt.Sprintf("error: a card with slug %q already exists. Use update_card instead.", a.TopicSlug)
	}

	// Run classifier gate if available. Fail-open if ClassifierLLM is nil.
	if ctx.ClassifierLLM != nil {
		writeType := "memory"
		if a.Subject == "self" {
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
			log.Infof("create_card rejected: %s — %s", verdict.Type, verdict.Reason)
			return classifier.RejectionMessage(verdict)
		}

		// Self-memory safety gate.
		if a.Subject == "self" {
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

	card, err := ctx.Store.CreateCard(a.TopicSlug, a.Name, a.Content, a.Subject, 0)
	if err != nil {
		log.Error("create_card failed", "err", err)
		return fmt.Sprintf("error: %v", err)
	}

	log.Infof("created card %s (%s): %s", a.TopicSlug, a.Subject, a.Name)
	return fmt.Sprintf("Created card %q (%s) — %s", card.TopicSlug, card.Subject, card.Name)
}
