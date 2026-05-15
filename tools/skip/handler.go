// Package skip implements the skip tool — the introspection agent's
// escape hatch for turns with nothing worth reflecting on. Functionally
// identical to done (sets DoneCalled), but distinct for observability:
// traces and sim reports distinguish "ran and found nothing" from
// "ran and saved something."
package skip

import (
	"encoding/json"

	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/skip")

func init() {
	tools.Register("skip", Handle)
}

// Handle marks the turn as complete (same as done) but logs a skip-specific
// trace line so we can see the agent chose to pass on this turn.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		// Non-fatal — reason is optional.
		args.Reason = ""
	}

	ctx.DoneCalled = true

	if args.Reason != "" {
		log.Infof("  skip: %s", args.Reason)
	} else {
		log.Info("  skip: nothing to reflect on")
	}
	return "skipped — turn complete"
}
