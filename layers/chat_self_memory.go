package layers

// Layer 2.5: Self-Knowledge — auto-injected self-memories relevant to this turn.
//
// Unlike agent-passed memories (which the driver explicitly selects via
// recall_memories), self-memories are injected automatically using a semantic
// search keyed on the driver's think traces + reply instruction. This gives
// the chat model awareness of its own communication patterns and identity
// observations without requiring the driver to explicitly recall them.

import (
	"fmt"
	"strings"

	"her/memory"
)

func init() {
	Register(PromptLayer{
		Name:    "Self-Knowledge",
		Order:   250,
		Stream:  StreamChat,
		Builder: buildChatSelfMemory,
	})
}

func buildChatSelfMemory(ctx *LayerContext) LayerResult {
	if ctx.EmbedClient == nil || ctx.Store == nil {
		return LayerResult{}
	}

	// Build a semantic query from think traces + reply instruction.
	// These capture HOW Mira is approaching the turn, which maps better
	// to self-memories about communication patterns than the user's
	// raw message does.
	var queryParts []string
	queryParts = append(queryParts, ctx.ThinkTraces...)
	if ctx.ReplyInstruction != "" {
		queryParts = append(queryParts, ctx.ReplyInstruction)
	}
	if len(queryParts) == 0 {
		return LayerResult{}
	}

	query := strings.Join(queryParts, " ")
	// Cap query length to avoid embedding extremely long text.
	if len(query) > 1000 {
		query = query[:1000]
	}

	queryVec, err := ctx.EmbedClient.Embed(query)
	if err != nil {
		return LayerResult{}
	}

	memories, err := ctx.Store.SemanticSearchBySubject(queryVec, "self", 5)
	if err != nil || len(memories) == 0 {
		return LayerResult{}
	}

	var b strings.Builder
	b.WriteString("## What I Know About Myself\n\n")
	var injected []memory.InjectedMemory
	for _, m := range memories {
		fmt.Fprintf(&b, "- %s\n", m.Content)
		injected = append(injected, memory.InjectedMemory{
			ID:       m.ID,
			Content:  m.Content,
			Source:   "self-memory",
			Distance: m.Distance,
		})
	}

	return LayerResult{
		Content:          b.String(),
		Detail:           fmt.Sprintf("%d self-memories", len(memories)),
		InjectedMemories: injected,
	}
}
