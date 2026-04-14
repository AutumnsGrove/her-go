// Package no_action implements the no_action tool — an explicit skip
// signal. The agent calls this when it has considered a tool but
// decided not to take action. This is cleaner than just not calling
// anything, because it shows intent in the thinking trace.
package no_action

import "her/tools"

func init() {
	tools.Register("no_action", Handle)
}

// Handle returns a simple completion string. No side effects.
func Handle(_ string, _ *tools.Context) string {
	return "tool call complete, no action taken"
}
