// Package todoist_complete implements the todoist_complete tool — marks
// a Todoist task as done.
package todoist_complete

import (
	"encoding/json"
	"fmt"

	"her/integrate"
	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/todoist_complete")

func init() {
	tools.Register("todoist_complete", Handle)
}

// Handle marks a Todoist task as completed.
// For recurring tasks, this completes the current occurrence and Todoist
// automatically schedules the next one.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	if args.TaskID == "" {
		return "error: task_id is required"
	}

	client := integrate.NewTodoistClient(ctx.Cfg.Todoist.APIKey)
	if client == nil {
		return "Todoist is not configured. Add todoist.api_key to config.yaml."
	}

	if err := client.CompleteTask(args.TaskID); err != nil {
		log.Error("completing task", "task_id", args.TaskID, "err", err)
		return fmt.Sprintf("error completing task: %v", err)
	}

	log.Infof("  todoist_complete: id=%s", args.TaskID)

	return fmt.Sprintf("Task %s marked as completed.", args.TaskID)
}
