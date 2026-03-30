// Package get_current_time implements the get_current_time tool —
// returns the current date and time in the user's configured timezone.
// Simple but essential: without this, the agent has no idea if it's
// morning or midnight.
package get_current_time

import (
	"time"

	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/get_current_time")

func init() {
	tools.Register("get_current_time", Handle)
}

// Handle loads the user's timezone from config and returns a
// human-readable date/time string with day of week, full date,
// time, and timezone — everything the agent might need for
// time-based reasoning.
func Handle(_ string, ctx *tools.Context) string {
	loc, err := time.LoadLocation(ctx.Cfg.Scheduler.Timezone)
	if err != nil {
		loc = time.UTC
	}

	now := time.Now().In(loc)

	// Include day of week, full date, time, and timezone.
	result := now.Format("Monday, January 2, 2006 at 3:04 PM (MST)")
	log.Info("  get_current_time", "result", result)
	return result
}
