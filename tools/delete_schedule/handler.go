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

// Handle soft-deletes a user-created schedule by disabling it.
// The row remains in the database for auditing — use list_schedules
// with show_disabled=true to see cancelled schedules.
func Handle(argsJSON string, ctx *tools.Context) string {
	var a struct {
		TaskID int64 `json:"task_id"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "error: invalid arguments"
	}
	if a.TaskID == 0 {
		return "error: task_id is required"
	}

	// Verify it exists before disabling.
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
	if !existing.Enabled {
		return fmt.Sprintf("Schedule #%d (%q) is already disabled.", a.TaskID, existing.Name)
	}

	updates := map[string]any{"enabled": false}
	if err := ctx.Store.UpdateSchedulerTask(a.TaskID, updates); err != nil {
		log.Error("delete_schedule failed", "err", err)
		return fmt.Sprintf("error: %v", err)
	}

	log.Infof("disabled schedule #%d: %s", a.TaskID, existing.Name)
	return fmt.Sprintf("Schedule #%d (%q) has been disabled. It will no longer fire.", a.TaskID, existing.Name)
}
