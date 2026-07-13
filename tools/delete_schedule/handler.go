package delete_schedule

import (
	"encoding/json"
	"fmt"
	"strconv"

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
	// Try parsing as int first, then string (LLMs sometimes send "35" instead of 35)
	var a struct {
		TaskID json.RawMessage `json:"task_id"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		log.Error("delete_schedule: JSON unmarshal failed", "input", argsJSON, "err", err)
		return fmt.Sprintf("error: invalid arguments (failed to parse JSON: %v)", err)
	}

	var taskID int64
	// Try as integer first
	if err := json.Unmarshal(a.TaskID, &taskID); err != nil {
		// Try as string
		var taskIDStr string
		if err2 := json.Unmarshal(a.TaskID, &taskIDStr); err2 != nil {
			log.Error("delete_schedule: task_id must be int or string", "input", argsJSON)
			return "error: task_id must be a number or numeric string"
		}
		// Parse string to int
		var err3 error
		taskID, err3 = strconv.ParseInt(taskIDStr, 10, 64)
		if err3 != nil {
			log.Error("delete_schedule: failed to parse task_id string", "input", taskIDStr, "err", err3)
			return fmt.Sprintf("error: task_id '%s' is not a valid number", taskIDStr)
		}
	}

	if taskID == 0 {
		log.Warn("delete_schedule: task_id is zero", "input", argsJSON)
		return "error: task_id is required"
	}

	// Verify it exists before disabling.
	existing, err := ctx.Store.GetSchedulerTaskByID(taskID)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}
	if existing == nil {
		return fmt.Sprintf("error: schedule #%d not found", taskID)
	}
	if !isManagedKind(existing.Kind) {
		return fmt.Sprintf("error: schedule #%d is a system task and cannot be modified", taskID)
	}
	if !existing.Enabled {
		return fmt.Sprintf("Schedule #%d (%q) is already disabled.", taskID, existing.Name)
	}

	updates := map[string]any{"enabled": false}
	if err := ctx.Store.UpdateSchedulerTask(taskID, updates); err != nil {
		log.Error("delete_schedule failed", "err", err)
		return fmt.Sprintf("error: %v", err)
	}

	log.Infof("disabled schedule #%d: %s", taskID, existing.Name)
	return fmt.Sprintf("Schedule #%d (%q) has been disabled. It will no longer fire.", taskID, existing.Name)
}

var managedKinds = map[string]bool{
	"worker_briefing": true,
	"send_message":    true,
	"send_prompt":     true,
}

func isManagedKind(kind string) bool { return managedKinds[kind] }
