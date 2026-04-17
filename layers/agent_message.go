package layers

// Agent layer: Current user message.
// The user's actual message for this turn, shown separately from
// history so the agent can distinguish "what they just said" from
// "what was said before".

import "fmt"

func init() {
	Register(PromptLayer{
		Name:    "Current Message",
		Order:   300,
		Stream:  StreamAgent,
		Builder: buildAgentMessage,
	})
}

func buildAgentMessage(ctx *LayerContext) LayerResult {
	content := fmt.Sprintf("## Current Message\n\n%s", ctx.ScrubbedUserMessage)
	return LayerResult{Content: content}
}
