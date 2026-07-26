package layers

// Agent layer: Self-Memory Index.
//
// Unlike user memories (open-ended, browsed via the Card Index above), the
// self-memory set is small and actively curated by the dream cycle
// (merge/expire) — see docs/plans/PLAN-memory-hooks-dual-recall.md. That
// makes it the right size to show in full, every turn, as a flat list of
// hooks — no card-browsing indirection needed. Historical D1 data showed
// self-memories were recalled in essentially 0 of the sampled turns despite
// real engineering investment in introspection/dreaming; this is the direct
// fix — the driver can't forget to consider what it already knows about
// itself if that's just sitting in context.
//
// Like the Card Index, this only shows hooks — recall_memories(id=...) or
// recall_memories(slug=...) is still required to pull the full memory
// content into the prompt.

import (
	"fmt"
	"strings"
)

func init() {
	Register(PromptLayer{
		Name:    "Self-Memory Index",
		Order:   385,
		Stream:  StreamAgent,
		Builder: buildSelfMemoryIndex,
	})
}

func buildSelfMemoryIndex(ctx *LayerContext) LayerResult {
	memories, err := ctx.Store.ActiveSelfMemories()
	if err != nil || len(memories) == 0 {
		return LayerResult{}
	}

	var b strings.Builder
	b.WriteString("## What I Know About Myself\n\n")
	b.WriteString("Self-observations from introspection. Skim for relevance, then call " +
		"recall_memories with id or slug to pull one in — no need to search if you saw the hook here.\n\n")

	for _, m := range memories {
		hook := strings.TrimSpace(m.Hook)
		if hook == "" {
			hook = "(no hook yet)"
		}
		if m.Slug != "" {
			fmt.Fprintf(&b, "- #%d [%s] %s\n", m.ID, m.Slug, hook)
		} else {
			fmt.Fprintf(&b, "- #%d %s\n", m.ID, hook)
		}
	}

	return LayerResult{
		Content: strings.TrimRight(b.String(), "\n"),
		Detail:  fmt.Sprintf("%d self-memories", len(memories)),
	}
}
