// Package think implements the think tool — the agent's internal
// reasoning scratchpad. It does nothing except give the model space
// to deliberate before making a decision. The thought text appears
// in the thinking trace but never reaches the user.
package think

import (
	"encoding/json"

	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/think")

func init() {
	tools.Register("think", Handle)
}

// Handle parses the thought argument and logs it. Returns a neutral
// completion string — NOT "ok", because the agent was interpreting
// "ok" as the user saying "ok" and getting into think→reply loops.
func Handle(argsJSON string, _ *tools.Context) string {
	var args struct {
		Thought string `json:"thought"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "tool call complete"
	}

	log.Infof("  think: %s", args.Thought)
	return "tool call complete"
}
