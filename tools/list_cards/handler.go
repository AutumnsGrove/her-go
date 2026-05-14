// Package list_cards implements the list_cards tool — shows all memory topic
// cards with their slugs, names, and summary previews.
package list_cards

import (
	"encoding/json"
	"fmt"
	"strings"

	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/list_cards")

func init() {
	tools.Register("list_cards", Handle)
}

type args struct {
	Subject string `json:"subject"`
}

// Handle returns a formatted list of all memory cards.
func Handle(argsJSON string, ctx *tools.Context) string {
	var a args
	_ = json.Unmarshal([]byte(argsJSON), &a)

	var cards []struct {
		Slug    string
		Name    string
		Subject string
		Summary string
		Version int
	}

	var err error
	if a.Subject != "" {
		raw, e := ctx.Store.CardsBySubject(a.Subject)
		err = e
		for _, c := range raw {
			cards = append(cards, struct {
				Slug    string
				Name    string
				Subject string
				Summary string
				Version int
			}{c.TopicSlug, c.Name, c.Subject, c.Summary, c.Version})
		}
	} else {
		raw, e := ctx.Store.AllCards()
		err = e
		for _, c := range raw {
			cards = append(cards, struct {
				Slug    string
				Name    string
				Subject string
				Summary string
				Version int
			}{c.TopicSlug, c.Name, c.Subject, c.Summary, c.Version})
		}
	}
	if err != nil {
		log.Error("list_cards failed", "err", err)
		return fmt.Sprintf("error: %v", err)
	}

	if len(cards) == 0 {
		return "No memory cards found."
	}

	var b strings.Builder
	fmt.Fprintf(&b, "%d cards:\n\n", len(cards))
	for _, c := range cards {
		preview := c.Summary
		if len(preview) > 100 {
			preview = preview[:100] + "..."
		}
		if preview == "" {
			preview = "(no summary yet)"
		}
		fmt.Fprintf(&b, "- **%s** [%s, v%d] %s\n  %s\n", c.Slug, c.Subject, c.Version, c.Name, preview)
	}
	return b.String()
}
