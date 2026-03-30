// Package update_schedule implements the update_schedule tool — pauses or
// resumes an existing scheduled task.
//
// Only the enabled/disabled state can be changed here. To modify a schedule's
// cron expression, task type, or payload, delete it and create a new one.
package update_schedule

import (
	"encoding/json"
	"fmt"

	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/update_schedule")

func init() {
	tools.Register("update_schedule", Handle)
}

// Handle toggles the enabled state of a scheduled task by ID.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		TaskID  int64 `json:"task_id"`
		Enabled bool  `json:"enabled"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	if args.TaskID == 0 {
		return "error: task_id is required"
	}

	if err := ctx.Store.UpdateScheduledTaskEnabled(args.TaskID, args.Enabled); err != nil {
		return fmt.Sprintf("error updating schedule: %v", err)
	}

	action := "paused"
	if args.Enabled {
		action = "resumed"
	}

	log.Info("agent updated schedule", "id", args.TaskID, "enabled", args.Enabled)
	return fmt.Sprintf("Schedule #%d %s.", args.TaskID, action)
}
