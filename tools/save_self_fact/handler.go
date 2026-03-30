// Package save_self_fact implements the save_self_fact tool — saves a fact
// about the bot herself (her own observations, identity, communication patterns).
//
// Shares its core logic with save_fact via tools.ExecSaveFact, but passes
// "self" as the subject. Self-facts have an extra quality gate (selfFactBlocklist)
// that blocks system-prompt restatements like "I can recall information."
package save_self_fact

import (
	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/save_self_fact")

func init() {
	tools.Register("save_self_fact", Handle)
}

// Handle delegates to the shared ExecSaveFact implementation with subject="self".
// "self" facts are about the bot's own identity, patterns, and observations —
// not about the user. These get stored with subject="self" in the facts table
// and displayed in a separate section of the system prompt.
func Handle(argsJSON string, ctx *tools.Context) string {
	log.Infof("  save_self_fact: %s", argsJSON)
	return tools.ExecSaveFact(argsJSON, "self", ctx)
}
