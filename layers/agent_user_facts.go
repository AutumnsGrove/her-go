package layers

// Agent layer: Memory recall prompt.
//
// The chat model has NO automatic memory injection. The agent is the sole
// gatekeeper — it calls recall_memories, evaluates results, and passes
// relevant ones to the chat model via reply(memories=[...]). If the agent
// skips recall, the chat model has zero memory context.

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
	return LayerResult{
		Content: fmt.Sprintf(
			"## Memory\n\n"+
				"The chat model has NO memory unless you provide it. "+
				"`recall_memories` is dual-mode:\n\n"+
				"- If a card/self-memory hook above looks relevant, fetch it directly: "+
				"`recall_memories(id=502)` or `recall_memories(slug=\"donette-bit\")`. "+
				"Cheap, exact, no ranking — prefer this.\n"+
				"- Only fall back to `recall_memories(query=\"...\")` when nothing in the "+
				"visible index already tells you which memory to fetch.\n\n"+
				"Either way, pass relevant results to `reply` via the `memories` parameter. "+
				"Skip recall only for bare greetings or trivial acknowledgements.",
		),
		Detail: "memory recall prompt",
	}
}
