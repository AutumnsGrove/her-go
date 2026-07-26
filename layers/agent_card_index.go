package layers

// Agent layer: Card Index.
//
// Experimental (branch experiment/card-index-recall). Prior to this layer,
// the driver only saw memory cards if it chose to call the list_cards tool —
// and in practice it almost never did, so recall_memories ran "blind": a bare
// KNN query against every memory, with no sense of what topics even exist.
//
// This layer makes the card index always-visible, the same way this
// project's own memory index (MEMORY.md) is unconditionally loaded into
// context every session instead of requiring an explicit search first. The
// idea: browsing a short list of topic summaries is a judgment call a model
// is good at ("does this look relevant to what we're discussing?"), whereas
// embedding similarity on a bare query is a numeric proxy that — per the
// card-index-canary experiment — didn't reliably predict which memories
// actually got used in a reply.
//
// This layer only shows the index (card slug + name + summary). It does not
// inject any memory content — recall_memories (optionally scoped via
// card_slug) is still required to pull actual memories into the prompt.

import (
	"fmt"
	"strings"

	"her/memory"
)

func init() {
	Register(PromptLayer{
		Name:    "Card Index",
		Order:   380,
		Stream:  StreamAgent,
		Builder: buildCardIndex,
	})
}

func buildCardIndex(ctx *LayerContext) LayerResult {
	cards, err := ctx.Store.AllCards()
	if err != nil || len(cards) == 0 {
		return LayerResult{}
	}

	return LayerResult{
		Content: RenderCardIndex(cards),
		Detail:  fmt.Sprintf("%d cards", len(cards)),
	}
}

// RenderCardIndex formats the card list (slug, name, summary) the same way
// for every consumer — the driver's always-visible layer above, and the
// memory agent's hand-rolled transcript (agent/memory_agent.go), which
// otherwise reimplemented this exact formatting separately. One rendering,
// so a change here (e.g. Phase 5's hook-list summaries) reaches both without
// the two views silently drifting apart.
func RenderCardIndex(cards []memory.MemoryCard) string {
	if len(cards) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("## Memory Cards\n\n")
	b.WriteString("Topics your memories are organized under. Skim for relevance, then call " +
		"recall_memories with `card_slug` set to search inside a specific card, or omit " +
		"card_slug for a global search.\n\n")

	var userLines, selfLines []string
	for _, c := range cards {
		summary := strings.TrimSpace(c.Summary)
		if summary == "" {
			summary = "(no summary yet)"
		}
		line := fmt.Sprintf("- [%s] %s — %s", c.TopicSlug, c.Name, summary)
		if c.Subject == "self" {
			selfLines = append(selfLines, line)
		} else {
			userLines = append(userLines, line)
		}
	}

	if len(userLines) > 0 {
		b.WriteString("About the user:\n")
		b.WriteString(strings.Join(userLines, "\n"))
		b.WriteString("\n")
	}
	if len(selfLines) > 0 {
		if len(userLines) > 0 {
			b.WriteString("\n")
		}
		b.WriteString("About myself:\n")
		b.WriteString(strings.Join(selfLines, "\n"))
	}

	return b.String()
}
