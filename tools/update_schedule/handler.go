package update_schedule

import (
	"encoding/json"
	"fmt"
	"time"

	"her/logger"
	"her/scheduler"
	"her/tools"
)

var log = logger.WithPrefix("tools/update_schedule")

func init() {
	tools.Register("update_schedule", Handle)
}

type args struct {
	TaskID  int64           `json:"task_id"`
	Name    *string         `json:"name"`
	CronExpr *string       `json:"cron_expr"`
	Enabled *bool           `json:"enabled"`
	Payload json.RawMessage `json:"payload"`
}

// Handle updates an existing user-created schedule. At least one
// optional field (name, cron_expr, enabled, payload) must be provided.
func Handle(argsJSON string, ctx *tools.Context) string {
	var a args
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "error: invalid arguments"
	}
	if a.TaskID == 0 {
		return "error: task_id is required"
	}

	// Verify the task exists before updating.
	existing, err := ctx.Store.GetSchedulerTaskByID(a.TaskID)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	if existing == nil {
		return fmt.Sprintf("error: schedule #%d not found", a.TaskID)
	}
	if existing.Name == "" {
		return fmt.Sprintf("error: schedule #%d is a system task and cannot be modified", a.TaskID)
	}

	updates := make(map[string]any)

	if a.Name != nil {
		updates["name"] = *a.Name
	}
	if a.Enabled != nil {
		updates["enabled"] = *a.Enabled
	}
	if len(a.Payload) > 0 && string(a.Payload) != "null" {
		if err := scheduler.ValidatePayload(existing.Kind, a.Payload); err != nil {
			return fmt.Sprintf("error: %s", err)
		}
		updates["payload_json"] = string(a.Payload)
	}
	if a.CronExpr != nil {
		if err := scheduler.ValidateCron(*a.CronExpr); err != nil {
			return fmt.Sprintf("error: %v", err)
		}

		// Recompute next fire time from the new cron.
		loc := time.UTC
		if ctx.Cfg != nil && ctx.Cfg.Timezone() != "" {
			if parsed, err := time.LoadLocation(ctx.Cfg.Timezone()); err == nil {
				loc = parsed
			}
		}

		nextFire, err := scheduler.NextRun(*a.CronExpr, time.Now(), loc)
		if err != nil {
			return fmt.Sprintf("error computing next fire time: %v", err)
		}

		updates["cron_expr"] = *a.CronExpr
		updates["next_fire"] = nextFire
	}

	if len(updates) == 0 {
		return "error: at least one field (name, cron_expr, enabled, payload) must be provided"
	}

	if err := ctx.Store.UpdateSchedulerTask(a.TaskID, updates); err != nil {
		log.Error("update_schedule failed", "err", err)
		return fmt.Sprintf("error: %v", err)
	}

	// Re-fetch to show the updated state.
	updated, err := ctx.Store.GetSchedulerTaskByID(a.TaskID)
	if err != nil || updated == nil {
		return fmt.Sprintf("Schedule #%d updated.", a.TaskID)
	}

	loc := time.UTC
	if ctx.Cfg != nil && ctx.Cfg.Timezone() != "" {
		if parsed, err := time.LoadLocation(ctx.Cfg.Timezone()); err == nil {
			loc = parsed
		}
	}

	status := "active"
	if !updated.Enabled {
		status = "DISABLED"
	}

	humanCron := scheduler.DescribeCron(updated.CronExpr)
	log.Infof("updated schedule #%d: %s", a.TaskID, updated.Name)
	return fmt.Sprintf(
		"Schedule #%d updated: %q\nSchedule: %s (%s)\nNext run: %s\nStatus: %s",
		updated.ID, updated.Name,
		humanCron, updated.CronExpr,
		updated.NextFire.In(loc).Format("Mon Jan 2 3:04 PM"),
		status,
	)
}

