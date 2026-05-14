// Package read_card implements the read_card tool — reads a memory card's
// summary and all its child memories. Used by the dream cycle to review
// a card's full contents.
package read_card

import (
	"encoding/json"
	"fmt"
	"strings"

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

// Handle returns a card's summary plus all its child memories.
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

	// Build header with card metadata.
	var b strings.Builder
	summary := card.Summary
	if summary == "" {
		summary = "(no summary yet)"
	}
	fmt.Fprintf(&b, "[%s] %s (v%d, %s)\nSummary: %s\n",
		card.TopicSlug, card.Name, card.Version, card.Subject, summary)

	// Load child memories.
	memories, err := ctx.Store.MemoriesByCard(card.ID)
	if err != nil {
		log.Error("read_card memories failed", "err", err)
		return fmt.Sprintf("error loading memories: %v", err)
	}

	if len(memories) == 0 {
		b.WriteString("\nNo memories in this card yet.")
	} else {
		fmt.Fprintf(&b, "\n%d memories:\n", len(memories))
		for _, m := range memories {
			fmt.Fprintf(&b, "- [#%d] %s\n", m.ID, m.Content)
		}
	}

	return b.String()
}
