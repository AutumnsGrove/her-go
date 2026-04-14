// Package list_schedules implements the list_schedules tool — returns a
// formatted list of all active scheduled tasks with their next run times.
//
// Cron expressions are converted to human-readable descriptions (e.g.,
// "30 9 * * *" → "daily at 9:30 AM") so the agent doesn't have to parse
// cron syntax. This prevents misreading like "30 9" being taken as 3pm.
package list_schedules

import (
	"fmt"
	"time"

	"her/logger"
	"her/scheduler"
	"her/tools"
)

var log = logger.WithPrefix("tools/list_schedules")

func init() {
	tools.Register("list_schedules", Handle)
}

// Handle fetches all active tasks and formats them for the agent. The
// argsJSON parameter is ignored — this tool takes no arguments.
func Handle(_ string, ctx *tools.Context) string {
	tasks, err := ctx.Store.ListActiveTasks()
	if err != nil {
		return fmt.Sprintf("error listing schedules: %v", err)
	}

	if len(tasks) == 0 {
		return "No active scheduled tasks."
	}

	// Load the configured timezone for formatting next-run times.
	// time.LoadLocation is Go's equivalent of Python's pytz.timezone().
	// Falls back to UTC if the timezone string is invalid.
	loc, _ := time.LoadLocation(ctx.Cfg.Scheduler.Timezone)
	if loc == nil {
		loc = time.UTC
	}

	result := fmt.Sprintf("Active scheduled tasks (%d):\n", len(tasks))
	for _, t := range tasks {
		name := "<unnamed>"
		if t.Name != nil {
			name = *t.Name
		}

		nextStr := "not scheduled"
		if t.NextRun != nil {
			nextStr = t.NextRun.In(loc).Format("Mon Jan 2 at 3:04 PM")
		}

		// Convert cron to human-readable so the agent doesn't have to
		// parse cron syntax (Trinity was misreading "30 9 * * *" as 3pm).
		scheduleStr := ""
		if t.CronExpr != nil && *t.CronExpr != "" {
			scheduleStr = fmt.Sprintf(" | Schedule: %s", scheduler.DescribeCron(*t.CronExpr))
		}

		result += fmt.Sprintf("\n#%d %s\n  Type: %s%s | Priority: %s | Next: %s | Runs: %d",
			t.ID, name, t.TaskType, scheduleStr, t.Priority, nextStr, t.RunCount)
	}

	log.Infof("  list_schedules: %d active tasks", len(tasks))
	return result
}
