package layers

// Agent layer: Footer instruction.
// A brief directive at the end of the agent context telling it
// what to do with all the information above.

func init() {
	Register(PromptLayer{
		Name:    "Agent Footer",
		Order:   900,
		Stream:  StreamAgent,
		Builder: buildAgentFooter,
	})
}

func buildAgentFooter(ctx *LayerContext) LayerResult {
	return LayerResult{
		Content: "Decide what to do: search if needed, then reply, then manage memory if appropriate.",
	}
}
