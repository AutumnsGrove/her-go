// Package done implements the done tool — the signal that the agent
// has finished all actions for this conversation turn. Every turn
// must end with a done call (after at least one reply).
package done

import (
	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/done")

func init() {
	tools.Register("done", Handle)
}

// Handle marks the turn as complete by setting DoneCalled on the context.
// The agent loop checks this flag to know when to stop iterating.
func Handle(_ string, ctx *tools.Context) string {
	ctx.DoneCalled = true
	log.Info("  done called — finishing turn")
	return "tool call complete, turn complete"
}
