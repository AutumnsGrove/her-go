package layers

// Agent layer: Memory recall prompt.
//
// The agent no longer receives auto-injected facts. Instead it uses
// recall_memories explicitly when it needs to look something up. This
// means the agent decides what to search for and when — rather than
// passively receiving whatever the user's message happened to match at
// turn start.
//
// Facts the agent retrieves via recall_memories should be passed through
// to the chat model via reply(facts=[...]). The chat model will inject
// exactly those facts into its context.

import "fmt"

func init() {
	Register(PromptLayer{
		Name:    "Memory Recall Prompt",
		Order:   400,
		Stream:  StreamAgent,
		Builder: buildAgentMemoryPrompt,
	})
}

func buildAgentMemoryPrompt(ctx *LayerContext) LayerResult {
	botName := ctx.Cfg.Identity.Her
	_ = botName

	return LayerResult{
		Content: fmt.Sprintf(
			"## Memory\n\n"+
				"Use `recall_memories` to search for relevant facts before replying. "+
				"Pass the facts you find to `reply` via the `facts` parameter — "+
				"the chat model will inject exactly those into its context.\n\n"+
				"If no facts are relevant, pass an empty `facts` list.",
		),
		Detail: "memory recall prompt",
	}
}
