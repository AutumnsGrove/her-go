package gateway

import (
	"context"

	"her/bot"
)

// buildCommands creates the gateway-level CommandDefs from a Pipeline.
func buildCommands(p *Pipeline) []CommandDef {
	return buildCommandsFromBot(p.Engine())
}

// buildCommandsFromBot creates CommandDefs from a bot.Bot instance.
// Each command wraps a transport-neutral Exec* method, making it
// available to every adapter (Telegram, Gradio, future Discord, etc.).
//
// Commands that are adapter-specific (like /clear, which resets
// adapter-local conversation state) are NOT included here — each
// adapter handles those directly.
//
// Commands that are Telegram-specific (/update, /restart, /mood wizard)
// stay registered with telebot directly.
func buildCommandsFromBot(bot *bot.Bot) []CommandDef {

	return []CommandDef{
		{
			Name:        "help",
			Description: "Show available commands",
			Handler: func(_ context.Context, _ string) (string, error) {
				return bot.ExecHelp(), nil
			},
		},
		{
			Name:        "stats",
			Description: "Show usage statistics",
			Handler: func(_ context.Context, _ string) (string, error) {
				return bot.ExecStats()
			},
		},
		{
			Name:        "facts",
			Description: "List all active memories",
			Handler: func(_ context.Context, _ string) (string, error) {
				return bot.ExecFacts()
			},
		},
		{
			Name:        "forget",
			Description: "Deactivate a memory by ID",
			Handler: func(_ context.Context, args string) (string, error) {
				return bot.ExecForget(args)
			},
		},
		{
			Name:        "traces",
			Description: "Toggle agent thinking traces",
			Handler: func(_ context.Context, _ string) (string, error) {
				return bot.ExecTraces()
			},
		},
		{
			Name:        "status",
			Description: "Show system status",
			Handler: func(_ context.Context, _ string) (string, error) {
				return bot.ExecStatus(), nil
			},
		},
		{
			Name:        "reflect",
			Description: "Trigger a manual reflection",
			Handler: func(_ context.Context, _ string) (string, error) {
				return bot.ExecReflect()
			},
		},
		{
			Name:        "reflections",
			Description: "Show recent reflections",
			Handler: func(_ context.Context, _ string) (string, error) {
				return bot.ExecReflections()
			},
		},
		{
			Name:        "persona",
			Description: "Show persona (subcommands: traits, history, rewrite)",
			Handler: func(_ context.Context, args string) (string, error) {
				return bot.ExecPersona(args)
			},
		},
		{
			Name:        "dream",
			Description: "Run a full dream cycle",
			Handler: func(_ context.Context, _ string) (string, error) {
				return bot.ExecDream()
			},
		},
		{
			Name:        "dreamlog",
			Description: "Show recent dream audit entries",
			Handler: func(_ context.Context, _ string) (string, error) {
				return bot.ExecDreamLog()
			},
		},
		{
			Name:        "lasttrace",
			Description: "Show the last turn's trace snapshot",
			Handler: func(_ context.Context, _ string) (string, error) {
				return bot.ExecLastTrace(), nil
			},
		},
	}
}
