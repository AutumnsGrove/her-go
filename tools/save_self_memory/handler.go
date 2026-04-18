// Package save_self_memory implements the save_self_memory tool — saves a memory
// about the bot herself (her own observations, identity, communication patterns).
//
// Shares its core logic with save_memory via tools.ExecSaveMemory, but passes
// "self" as the subject.
package save_self_memory

import (
	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/save_self_memory")

func init() {
	tools.Register("save_self_memory", Handle)
}

// Handle delegates to the shared ExecSaveMemory implementation with subject="self".
// "self" memories are about the bot's own identity, patterns, and observations —
// not about the user. These get stored with subject="self" in the memories table
// and displayed in a separate section of the system prompt.
func Handle(argsJSON string, ctx *tools.Context) string {
	log.Infof("  save_self_memory: %s", argsJSON)
	return tools.ExecSaveMemory(argsJSON, "self", ctx)
}
