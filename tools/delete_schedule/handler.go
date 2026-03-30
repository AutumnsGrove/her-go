// Package delete_schedule implements the delete_schedule tool — permanently
// removes a scheduled task by ID.
//
// Unlike remove_fact (soft delete), this is a hard delete — the task is gone.
// The agent should route this through reply_confirm first so the user can
// confirm the deletion before it's executed.
package delete_schedule

import (
	"encoding/json"
	"fmt"

	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/delete_schedule")

func init() {
	tools.Register("delete_schedule", Handle)
}

// Handle permanently deletes a scheduled task by ID.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		TaskID int64 `json:"task_id"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	if args.TaskID == 0 {
		return "error: task_id is required"
	}

	if err := ctx.Store.DeleteScheduledTask(args.TaskID); err != nil {
		return fmt.Sprintf("error deleting schedule: %v", err)
	}

	log.Info("agent deleted schedule", "id", args.TaskID)
	return fmt.Sprintf("Schedule #%d deleted.", args.TaskID)
}
