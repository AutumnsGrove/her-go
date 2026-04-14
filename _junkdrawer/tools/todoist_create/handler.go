// Package todoist_create implements the todoist_create tool — creates a
// new task in the user's Todoist.
package todoist_create

import (
	"encoding/json"
	"fmt"
	"strings"

	"her/integrate"
	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/todoist_create")

func init() {
	tools.Register("todoist_create", Handle)
}

// Handle creates a new Todoist task from the agent's arguments.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		Content     string `json:"content"`
		Description string `json:"description"`
		DueString   string `json:"due_string"`
		Priority    int    `json:"priority"`
		Labels      string `json:"labels"` // comma-separated string from the agent
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	if args.Content == "" {
		return "error: content (task title) is required"
	}

	client := integrate.NewTodoistClient(ctx.Cfg.Todoist.APIKey)
	if client == nil {
		return "Todoist is not configured. Add todoist.api_key to config.yaml."
	}

	// Parse comma-separated labels into a slice.
	// strings.FieldsFunc splits on commas and trims whitespace — more
	// robust than strings.Split which would leave empty strings for
	// inputs like "work, ,urgent".
	var labels []string
	if args.Labels != "" {
		for _, l := range strings.Split(args.Labels, ",") {
			l = strings.TrimSpace(l)
			if l != "" {
				labels = append(labels, l)
			}
		}
	}

	task, err := client.CreateTask(args.Content, args.Description, args.DueString, args.Priority, "", labels)
	if err != nil {
		log.Error("creating task", "content", args.Content, "err", err)
		return fmt.Sprintf("error creating task: %v", err)
	}

	log.Infof("  todoist_create: %q (id=%s)", task.Content, task.ID)

	result := fmt.Sprintf("Task created: %s (ID=%s)", task.Content, task.ID)
	if task.Due != nil {
		result += fmt.Sprintf(", due: %s", task.Due.Date)
	}
	if task.URL != "" {
		result += fmt.Sprintf("\nLink: %s", task.URL)
	}

	return result
}
