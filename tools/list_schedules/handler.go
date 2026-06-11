package list_schedules

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"her/logger"
	"her/scheduler"
	"her/tools"
)

var log = logger.WithPrefix("tools/list_schedules")

func init() {
	tools.Register("list_schedules", Handle)
}

// Handle lists all user-created scheduled tasks.
func Handle(argsJSON string, ctx *tools.Context) string {
	var a struct {
		ShowDisabled bool `json:"show_disabled"`
	}
	if argsJSON != "" {
		if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
			return "error: invalid arguments"
		}
	}

	tasks, err := ctx.Store.ListUserSchedulerTasks(a.ShowDisabled)
	if err != nil {
		log.Error("list_schedules failed", "err", err)
		return fmt.Sprintf("error: %v", err)
	}

	if len(tasks) == 0 {
		if a.ShowDisabled {
			return "No scheduled tasks found (including disabled)."
		}
		return "No active scheduled tasks."
	}

	// Resolve timezone for display.
	loc := time.UTC
	if ctx.Cfg != nil && ctx.Cfg.Timezone() != "" {
		if parsed, err := time.LoadLocation(ctx.Cfg.Timezone()); err == nil {
			loc = parsed
		}
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Scheduled tasks (%d):\n\n", len(tasks)))

	for _, t := range tasks {
		status := "active"
		if !t.Enabled {
			status = "DISABLED"
		}

		humanCron := scheduler.DescribeCron(t.CronExpr)
		name := t.Name
		if name == "" {
			name = "<unnamed>"
		}

		sb.WriteString(fmt.Sprintf(
			"#%d %s [%s]\n  Type: %s | Schedule: %s (%s)\n  Next: %s | Status: %s\n",
			t.ID, name, t.Kind,
			t.Kind, humanCron, t.CronExpr,
			t.NextFire.In(loc).Format("Mon Jan 2 3:04 PM"),
			status,
		))

		// Payload summary.
		summary := payloadSummary(t.Kind, t.Payload)
		if summary != "" {
			sb.WriteString(fmt.Sprintf("  Payload: %s\n", summary))
		}
		sb.WriteString("\n")
	}

	return sb.String()
}

// payloadSummary returns a short description of the payload for display.
func payloadSummary(kind string, payload json.RawMessage) string {
	if len(payload) == 0 || string(payload) == "{}" {
		return ""
	}

	var p map[string]any
	if err := json.Unmarshal(payload, &p); err != nil {
		return ""
	}

	switch kind {
	case "worker_briefing":
		parts := []string{}
		if topics, ok := p["topics"].(string); ok && topics != "" {
			parts = append(parts, "topics: "+topics)
		}
		if depth, ok := p["depth"].(string); ok && depth != "" {
			parts = append(parts, "depth: "+depth)
		}
		return strings.Join(parts, ", ")
	case "send_message":
		if msg, ok := p["message"].(string); ok {
			if len(msg) > 60 {
				msg = msg[:57] + "..."
			}
			return fmt.Sprintf("message: %q", msg)
		}
	case "send_prompt":
		if prompt, ok := p["prompt"].(string); ok {
			if len(prompt) > 60 {
				prompt = prompt[:57] + "..."
			}
			return fmt.Sprintf("prompt: %q", prompt)
		}
	}
	return ""
}
