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
// Normally this only shows hooks — recall_memories(id=...) or
// recall_memories(slug=...) is still required to pull the full memory
// content into the prompt. That's fine under the standard reply tool,
// because the chat model gets full self-memory content anyway via a
// separate semantic auto-injection (layers/chat_self_memory.go). But when
// cfg.Driver.DirectReply is on, the driver's own words ARE the reply — there
// is no second model to catch what the driver's hook-skimming missed. So in
// that mode this layer renders full content instead of just hooks. We chose
// "always full" over "semantically inject relevant ones" deliberately:
// semantic matching can miss a memory that's relevant in a way the
// embedding doesn't capture, or surface one that isn't actually relevant.
// Since the self-memory set is small and curated, showing all of it in full
// avoids that failure mode entirely — this is an experiment, gated behind
// the same flag, so standard-reply-tool behavior is untouched.
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

	fullContent := ctx.Cfg != nil && ctx.Cfg.Driver.DirectReply

	var b strings.Builder
	b.WriteString("## What I Know About Myself\n\n")
	if fullContent {
		b.WriteString("Self-observations from introspection — shown in full because your words " +
			"are the reply this turn, with no second model to catch what you don't recall.\n\n")
	} else {
		b.WriteString("Self-observations from introspection. Skim for relevance, then call " +
			"recall_memories with id or slug to pull one in — no need to search if you saw the hook here.\n\n")
	}

	for _, m := range memories {
		label := strings.TrimSpace(m.Hook)
		if fullContent {
			label = strings.TrimSpace(m.Content)
		}
		if label == "" {
			label = "(no hook yet)"
		}
		if m.Slug != "" {
			fmt.Fprintf(&b, "- #%d [%s] %s\n", m.ID, m.Slug, label)
		} else {
			fmt.Fprintf(&b, "- #%d %s\n", m.ID, label)
		}
	}

	return LayerResult{
		Content: strings.TrimRight(b.String(), "\n"),
		Detail:  fmt.Sprintf("%d self-memories", len(memories)),
	}
}
