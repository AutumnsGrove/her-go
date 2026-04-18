package layers

// Layer 4: Memory context — memories injected into the chat model's prompt.
//
// Two sources, in priority order:
//
//  1. Agent-passed memories (ctx.AgentPassedMemories): the agent called recall_memories,
//     evaluated the results, and explicitly passed the relevant ones through the
//     reply tool's facts parameter. These represent the agent's curated judgment.
//     When present, they're injected verbatim — no redundancy filtering needed
//     since the agent already chose them deliberately.
//
//  2. Auto-searched memories (ctx.RelevantMemories): the KNN results from the turn-start
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

	// Path 1: agent explicitly passed memories via reply(facts=[...]).
	// Inject these directly — the agent already did the relevance judgment.
	if len(ctx.AgentPassedMemories) > 0 {
		var b strings.Builder
		b.WriteString("## What I Remember\n\n")
		for _, m := range ctx.AgentPassedMemories {
			fmt.Fprintf(&b, "- %s\n", m)
		}
		return LayerResult{
			Content: b.String(),
			Detail:  fmt.Sprintf("%d agent-passed memories", len(ctx.AgentPassedMemories)),
		}
	}

	// Path 2: fallback to auto-searched memories from turn-start KNN.
	// Filter out memories already represented in conversation history to
	// avoid "context echo" (model fixating on repeated information).
	filteredMemories := ctx.RelevantMemories
	if ctx.EmbedClient != nil {
		recentMsgs, err := ctx.Store.RecentMessages(ctx.ConversationID, ctx.Cfg.Memory.RecentMessages)
		if err == nil && len(recentMsgs) > 0 {
			filteredMemories = memory.FilterRedundantMemories(filteredMemories, recentMsgs, ctx.EmbedClient)
		}
	}

	memCtx, injectedMemories, err := memory.BuildMemoryContext(
		ctx.Store,
		ctx.Cfg.Memory.MaxFactsInContext,
		filteredMemories,
		ctx.Cfg.Identity.User,
		ctx.Cfg.Embed.MaxSemanticDistance,
	)
	if err != nil || memCtx == "" {
		return LayerResult{}
	}

	// Count by source for the detail string.
	semantic, linked := 0, 0
	for _, m := range injectedMemories {
		switch m.Source {
		case "semantic":
			semantic++
		case "linked":
			linked++
		}
	}

	detail := fmt.Sprintf("%d memories (auto)", len(injectedMemories))
	if linked > 0 {
		detail = fmt.Sprintf("%d semantic + %d linked (auto)", semantic, linked)
	}

	return LayerResult{
		Content:          memCtx,
		Detail:           detail,
		InjectedMemories: injectedMemories,
	}
}
