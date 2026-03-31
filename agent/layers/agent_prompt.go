package layers

// Agent layer: System prompt (agent_prompt.md).
// The foundational rules for the agent orchestrator. Hot-reloaded
// from disk on every turn.
//
// This is an "overhead" layer — it doesn't contribute to the user
// context message (Content stays empty), but it reports token usage
// so `her shape` shows the full picture. The actual system prompt
// assembly happens in agent.go's loadAgentPrompt.

import "os"

func init() {
	Register(PromptLayer{
		Name:    "agent_prompt.md (system)",
		Order:   10,
		Stream:  StreamAgent,
		Builder: buildAgentPrompt,
	})
}

func buildAgentPrompt(ctx *LayerContext) LayerResult {
	data, err := os.ReadFile(ctx.Cfg.Persona.AgentPromptFile)
	if err != nil || len(data) == 0 {
		return LayerResult{Detail: "missing"}
	}
	// Just estimate the raw file size. The actual prompt gets tool
	// inventory markers expanded, but the file size is close enough
	// for shape reporting.
	return LayerResult{
		Tokens: estimateTokens(string(data)),
	}
}
