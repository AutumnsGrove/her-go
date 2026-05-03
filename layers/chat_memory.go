package layers

// Layer 4: Memory context — memories the agent explicitly passed to the chat
// model via reply(memories=[...]).
//
// The agent calls recall_memories, evaluates the results, and passes relevant
// ones through. This is the ONLY path memories enter the chat prompt — there
// is no auto-injection fallback. If the agent didn't pass memories, the chat
// model gets no memory section (and that's correct — the agent decided it
// wasn't needed).

import (
	"fmt"
	"strings"
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
	if len(ctx.AgentPassedMemories) == 0 {
		return LayerResult{}
	}

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
