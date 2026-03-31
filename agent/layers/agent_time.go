package layers

// Agent layer: Current time.
// The agent needs this to convert natural language times ("in 2 hours",
// "tomorrow at 3pm") to absolute ISO timestamps for create_reminder.

import (
	"fmt"
	"time"
)

func init() {
	Register(PromptLayer{
		Name:    "Current Time",
		Order:   100,
		Stream:  StreamAgent,
		Builder: buildAgentTime,
	})
}

func buildAgentTime(ctx *LayerContext) LayerResult {
	loc, err := time.LoadLocation(ctx.Cfg.Scheduler.Timezone)
	if err != nil {
		loc = time.UTC
	}
	now := time.Now().In(loc)
	content := fmt.Sprintf("## Current Time\n\n%s (timezone: %s)",
		now.Format("2006-01-02T15:04:05 (Monday)"), loc.String())
	return LayerResult{Content: content}
}
