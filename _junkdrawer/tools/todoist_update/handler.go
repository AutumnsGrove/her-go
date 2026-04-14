// Package todoist_update implements the todoist_update tool — modifies
// an existing Todoist task's content, due date, priority, or description.
package todoist_update

import (
	"encoding/json"
	"fmt"

	"her/integrate"
	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/todoist_update")

func init() {
	tools.Register("todoist_update", Handle)
}

// Handle updates a Todoist task. Only sends fields that the agent provided
// — omitted fields keep their current values in Todoist.
func Handle(argsJSON string, ctx *tools.Context) string {
	// We parse into a map first to distinguish "field not provided" from
	// "field set to empty string". This matters for the update API —
	// sending content="" would clear the title, which is never what we want.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(argsJSON), &raw); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	var taskID string
	if v, ok := raw["task_id"]; ok {
		json.Unmarshal(v, &taskID)
	}
	if taskID == "" {
		return "error: task_id is required"
	}

	client := integrate.NewTodoistClient(ctx.Cfg.Todoist.APIKey)
	if client == nil {
		return "Todoist is not configured. Add todoist.api_key to config.yaml."
	}

	// Build pointer fields — nil means "don't change this field".
	// This is Go's way of handling optional/partial updates. In Python
	// you'd use None or a sentinel value; in Go, *string where nil = absent.
	var content, description, dueString *string
	var priority *int

	if v, ok := raw["content"]; ok {
		var s string
		json.Unmarshal(v, &s)
		if s != "" {
			content = &s
		}
	}
	if v, ok := raw["description"]; ok {
		var s string
		json.Unmarshal(v, &s)
		description = &s // allow empty string to clear description
	}
	if v, ok := raw["due_string"]; ok {
		var s string
		json.Unmarshal(v, &s)
		if s != "" {
			dueString = &s
		}
	}
	if v, ok := raw["priority"]; ok {
		var p int
		json.Unmarshal(v, &p)
		if p >= 1 && p <= 4 {
			priority = &p
		}
	}

	task, err := client.UpdateTask(taskID, content, description, dueString, priority, nil)
	if err != nil {
		log.Error("updating task", "task_id", taskID, "err", err)
		return fmt.Sprintf("error updating task: %v", err)
	}

	log.Infof("  todoist_update: id=%s → %q", task.ID, task.Content)

	result := fmt.Sprintf("Task updated: %s (ID=%s)", task.Content, task.ID)
	if task.Due != nil {
		result += fmt.Sprintf(", due: %s", task.Due.Date)
	}

	return result
}
