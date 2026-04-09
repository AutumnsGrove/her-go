package layers

// Layer 4: Memory context — semantically relevant facts.
//
// This is where the bot's long-term memory enters the chat prompt.
// Unlike the agent (which sees ALL facts), the chat model only sees
// facts that are semantically close to the current message (via KNN).
//
// Before injection, facts are filtered for redundancy against recent
// conversation history — if a fact says the same thing as a message
// the model already sees in history, it's dropped to prevent "context
// echo" (the model fixating on repeated information).

import (
	"fmt"

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

	// Filter out facts already represented in conversation history.
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

	detail := fmt.Sprintf("%d facts", len(injectedFacts))
	if linked > 0 {
		detail = fmt.Sprintf("%d semantic + %d linked", semantic, linked)
	}

	return LayerResult{
		Content:      memCtx,
		Detail:       detail,
		InjectedFacts: injectedFacts,
	}
}
