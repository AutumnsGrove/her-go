// Package save_fact implements the save_fact tool — saves a new fact about
// the user into the memory database.
//
// The actual logic lives in tools.ExecSaveFact (fact_helpers.go) which is
// shared with save_self_fact. This handler is a thin wrapper that passes
// "user" as the subject, meaning the fact is about the user (not about the bot).
package save_fact

import (
	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/save_fact")

func init() {
	// Register this handler with the central registry. The init() function
	// runs automatically when the package is imported — the blank import in
	// agent/agent.go (import _ "her/tools/save_fact") triggers it.
	tools.Register("save_fact", Handle)
}

// Handle delegates to the shared ExecSaveFact implementation with subject="user".
// "user" facts are facts about the person the bot is talking to (Autumn).
// Compare save_self_fact which uses subject="self" for facts about the bot itself.
func Handle(argsJSON string, ctx *tools.Context) string {
	log.Infof("  save_fact: %s", argsJSON)
	return tools.ExecSaveFact(argsJSON, "user", ctx)
}
