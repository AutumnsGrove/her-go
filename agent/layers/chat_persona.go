package layers

// Layer 2: Evolving self-image from persona.md.
// Written by the bot itself during persona rewrites. Contains the
// accumulated self-knowledge from reflections over time.

import "os"

func init() {
	Register(PromptLayer{
		Name:    "persona.md",
		Order:   200,
		Stream:  StreamChat,
		Builder: buildChatPersona,
	})
}

func buildChatPersona(ctx *LayerContext) LayerResult {
	data, err := os.ReadFile(ctx.Cfg.Persona.PersonaFile)
	if err != nil {
		return LayerResult{}
	}
	content := ctx.Cfg.ExpandPrompt(string(data))
	return LayerResult{Content: content}
}
