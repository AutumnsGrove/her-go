package layers

// Layer 1: Base identity from prompt.md.
// This is the foundational personality — who the bot IS. Hot-reloaded
// from disk on every turn so you can edit it without restarting.

import "os"

func init() {
	Register(PromptLayer{
		Name:    "prompt.md",
		Order:   100,
		Stream:  StreamChat,
		Builder: buildChatPrompt,
	})
}

func buildChatPrompt(ctx *LayerContext) LayerResult {
	data, err := os.ReadFile(ctx.Cfg.Persona.PromptFile)
	if err != nil {
		return LayerResult{}
	}
	content := ctx.Cfg.ExpandPrompt(string(data))
	return LayerResult{Content: content}
}
