package layers

// Layer 4: Memory context — facts injected into the chat model's prompt.
//
// Two sources, in priority order:
//
//  1. Agent-passed facts (ctx.AgentPassedFacts): the agent called recall_memories,
//     evaluated the results, and explicitly passed the relevant ones through the
//     reply tool's facts parameter. These represent the agent's curated judgment.
//     When present, they're injected verbatim — no redundancy filtering needed
//     since the agent already chose them deliberately.
//
//  2. Auto-searched facts (ctx.RelevantFacts): the KNN results from the turn-start
//     semantic search, used as a fallback when the agent passed nothing. Filtered
//     for redundancy against conversation history before injection.

import (
	"fmt"
	"strings"

	"her/memory"
)

func init() {
	Register(PromptLayer{
		Name:    "Memory Context",
		Order:   400,
		Stream:  StreamChat,
		Builder: buildChatMemory,
	})
}

func buildChatMemory(ctx *LayerContext) LayerResult {
	if ctx.Store == nil {
		return LayerResult{}
	}

	// Path 1: agent explicitly passed facts via reply(facts=[...]).
	// Inject these directly — the agent already did the relevance judgment.
	if len(ctx.AgentPassedFacts) > 0 {
		var b strings.Builder
		b.WriteString("## What I Remember\n\n")
		for _, f := range ctx.AgentPassedFacts {
			fmt.Fprintf(&b, "- %s\n", f)
		}
		return LayerResult{
			Content: b.String(),
			Detail:  fmt.Sprintf("%d agent-passed facts", len(ctx.AgentPassedFacts)),
		}
	}

	// Path 2: fallback to auto-searched facts from turn-start KNN.
	// Filter out facts already represented in conversation history to
	// avoid "context echo" (model fixating on repeated information).
	filteredFacts := ctx.RelevantFacts
	if ctx.EmbedClient != nil {
		recentMsgs, err := ctx.Store.RecentMessages(ctx.ConversationID, ctx.Cfg.Memory.RecentMessages)
		if err == nil && len(recentMsgs) > 0 {
			filteredFacts = memory.FilterRedundantFacts(filteredFacts, recentMsgs, ctx.EmbedClient)
		}
	}

	memCtx, injectedFacts, err := memory.BuildMemoryContext(
		ctx.Store,
		ctx.Cfg.Memory.MaxFactsInContext,
		filteredFacts,
		ctx.Cfg.Identity.User,
		ctx.Cfg.Embed.MaxSemanticDistance,
	)
	if err != nil || memCtx == "" {
		return LayerResult{}
	}

	// Count by source for the detail string.
	semantic, linked := 0, 0
	for _, f := range injectedFacts {
		switch f.Source {
		case "semantic":
			semantic++
		case "linked":
			linked++
		}
	}

	detail := fmt.Sprintf("%d facts (auto)", len(injectedFacts))
	if linked > 0 {
		detail = fmt.Sprintf("%d semantic + %d linked (auto)", semantic, linked)
	}

	return LayerResult{
		Content:       memCtx,
		Detail:        detail,
		InjectedFacts: injectedFacts,
	}
}
