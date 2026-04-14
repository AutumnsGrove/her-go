// Package todoist_list implements the todoist_list tool — retrieves active
// tasks from the user's Todoist account.
package todoist_list

import (
	"encoding/json"
	"fmt"

	"her/integrate"
	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/todoist_list")

func init() {
	tools.Register("todoist_list", Handle)
}

// Handle queries Todoist for active tasks, optionally filtered.
// The filter parameter uses Todoist's own filter syntax — the agent
// picks the right filter based on what the user asked.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		Filter string `json:"filter"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	// Construct the client from config. This is cheap — no state to share.
	client := integrate.NewTodoistClient(ctx.Cfg.Todoist.APIKey)
	if client == nil {
		return "Todoist is not configured. Add todoist.api_key to config.yaml."
	}

	tasks, err := client.ListTasks(args.Filter)
	if err != nil {
		log.Error("listing tasks", "filter", args.Filter, "err", err)
		return fmt.Sprintf("error listing tasks: %v", err)
	}

	filterDesc := args.Filter
	if filterDesc == "" {
		filterDesc = "all active"
	}
	log.Infof("  todoist_list: filter=%q → %d tasks", filterDesc, len(tasks))

	return integrate.FormatTasks(tasks)
}
