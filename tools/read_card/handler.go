// Package read_card implements the read_card tool — reads the full content
// of a memory card by topic slug.
package read_card

import (
	"encoding/json"
	"fmt"

	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/read_card")

func init() {
	tools.Register("read_card", Handle)
}

type args struct {
	TopicSlug string `json:"topic_slug"`
}

// Handle returns the full content of a memory card.
func Handle(argsJSON string, ctx *tools.Context) string {
	var a args
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "error: invalid arguments"
	}
	if a.TopicSlug == "" {
		return "error: topic_slug is required"
	}

	card, err := ctx.Store.GetCard(a.TopicSlug)
	if err != nil {
		log.Error("read_card failed", "err", err)
		return fmt.Sprintf("error: %v", err)
	}
	if card == nil {
		return fmt.Sprintf("error: no card found with slug %q", a.TopicSlug)
	}

	content := card.Content
	if content == "" {
		content = "(empty — no content yet)"
	}

	return fmt.Sprintf("[%s] %s (v%d, %s)\n\n%s",
		card.TopicSlug, card.Name, card.Version, card.Subject, content)
}
