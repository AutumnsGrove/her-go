// Package update_card implements the update_card tool — rewrites a memory
// card's dreamer-maintained summary. This is a dream-cycle-only operation;
// the real-time memory agent never calls this.
package update_card

import (
	"encoding/json"
	"fmt"

	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/update_card")

func init() {
	tools.Register("update_card", Handle)
}

type args struct {
	TopicSlug string `json:"topic_slug"`
	Summary   string `json:"summary"`
	Delta     string `json:"delta"`
}

// Handle rewrites a card's summary and logs the change.
func Handle(argsJSON string, ctx *tools.Context) string {
	var a args
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "error: invalid arguments"
	}
	if a.TopicSlug == "" {
		return "error: topic_slug is required"
	}
	if a.Summary == "" {
		return "error: summary is required"
	}
	if a.Delta == "" {
		return "error: delta is required"
	}

	card, err := ctx.Store.GetCard(a.TopicSlug)
	if err != nil {
		log.Error("update_card lookup failed", "err", err)
		return fmt.Sprintf("error: %v", err)
	}
	if card == nil {
		return fmt.Sprintf("error: no card found with slug %q", a.TopicSlug)
	}

	updated, err := ctx.Store.UpdateCardSummary(a.TopicSlug, a.Summary, a.Delta, 0)
	if err != nil {
		log.Error("update_card failed", "err", err)
		return fmt.Sprintf("error: %v", err)
	}

	log.Infof("updated card summary %s (v%d): %s", a.TopicSlug, updated.Version, a.Delta)
	return fmt.Sprintf("Updated card %q summary (v%d). Delta: %s", a.TopicSlug, updated.Version, a.Delta)
}
