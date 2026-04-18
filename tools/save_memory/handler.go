// Package save_memory implements the save_memory tool — saves a new memory about
// the user into the memory database.
//
// The actual logic lives in tools.ExecSaveMemory (fact_helpers.go) which is
// shared with save_self_memory. This handler is a thin wrapper that passes
// "user" as the subject, meaning the memory is about the user (not about the bot).
package save_memory

import (
	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/save_memory")

func init() {
	// Register this handler with the central registry. The init() function
	// runs automatically when the package is imported — the blank import in
	// agent/memory_agent.go (import _ "her/tools/save_memory") triggers it.
	tools.Register("save_memory", Handle)
}

// Handle delegates to the shared ExecSaveMemory implementation with subject="user".
// "user" memories are memories about the person the bot is talking to (Autumn).
// Compare save_self_memory which uses subject="self" for memories about the bot itself.
func Handle(argsJSON string, ctx *tools.Context) string {
	log.Infof("  save_memory: %s", argsJSON)
	return tools.ExecSaveMemory(argsJSON, "user", ctx)
}
