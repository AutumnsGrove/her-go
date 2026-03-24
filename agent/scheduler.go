package agent

import (
	"encoding/json"
	"fmt"
	"time"

	"her/memory"
	"her/scheduler"
)

// execCreateReminder handles the create_reminder agent tool.
// The agent has already parsed the user's natural language time into
// an ISO 8601 timestamp — we just validate it and create the task.
//
// This is different from /remind where WE parse the time. Here the
// LLM does the parsing (it's surprisingly good at it) and gives us
// a clean timestamp. We just need to make sure it's valid and in
// the future.
func execCreateReminder(argsJSON string, tctx *toolContext) string {
	var args struct {
		Message     string `json:"message"`
		TriggerAt   string `json:"trigger_at"`
		NaturalTime string `json:"natural_time"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	if args.Message == "" {
		return "error: message is required"
	}
	if args.TriggerAt == "" {
		return "error: trigger_at is required"
	}

	// Parse the ISO 8601 timestamp. time.Parse is Go's time parsing —
	// it uses a reference time layout instead of format codes like Python's
	// strftime. The layout "2006-01-02T15:04:05" is Go's magic reference
	// date (Jan 2, 2006 at 3:04:05 PM). Every number in the layout is
	// unique so Go can tell which part is which.
	//
	// Yes, it's weird. Yes, everyone says that. But once you know the
	// reference date it actually works pretty well.
	triggerAt, err := time.Parse("2006-01-02T15:04:05", args.TriggerAt)
	if err != nil {
		// Try with timezone offset too.
		triggerAt, err = time.Parse(time.RFC3339, args.TriggerAt)
		if err != nil {
			return fmt.Sprintf("error: couldn't parse trigger_at '%s' — expected ISO 8601 format like '2026-03-22T15:00:00'", args.TriggerAt)
		}
	}

	// Apply the configured timezone if the timestamp didn't include one.
	// A bare "2026-03-22T15:00:00" should mean 3pm in the USER's timezone,
	// not UTC.
	if triggerAt.Location() == time.UTC && args.TriggerAt[len(args.TriggerAt)-1] != 'Z' {
		loc, locErr := time.LoadLocation(tctx.cfg.Scheduler.Timezone)
		if locErr == nil {
			triggerAt = time.Date(
				triggerAt.Year(), triggerAt.Month(), triggerAt.Day(),
				triggerAt.Hour(), triggerAt.Minute(), triggerAt.Second(),
				0, loc,
			)
		}
	}

	// Sanity check: reminder should be in the future.
	if triggerAt.Before(time.Now()) {
		return fmt.Sprintf("error: trigger_at '%s' is in the past", args.TriggerAt)
	}

	// Build the task.
	taskName := "remind: " + args.Message
	if len(taskName) > 60 {
		taskName = taskName[:57] + "..."
	}

	payload, _ := json.Marshal(map[string]string{
		"message": args.Message,
	})

	maxRuns := 1
	task := &memory.ScheduledTask{
		Name:            &taskName,
		ScheduleType:    "once",
		TriggerAt:       &triggerAt,
		TaskType:        "send_message",
		Payload:         payload,
		Enabled:         true,
		NextRun:         &triggerAt,
		MaxRuns:         &maxRuns,
		Priority:        "critical", // user-requested reminders always fire, bypassing all damping
		CreatedBy:       "agent",
		SourceMessageID: &tctx.triggerMsgID,
	}

	id, err := tctx.store.CreateScheduledTask(task)
	if err != nil {
		return fmt.Sprintf("error creating reminder: %v", err)
	}

	timeDisplay := triggerAt.Format("Mon Jan 2 at 3:04 PM")
	log.Info("agent created reminder", "id", id, "trigger_at", triggerAt, "message", args.Message)

	return fmt.Sprintf("Reminder #%d created: '%s' at %s", id, args.Message, timeDisplay)
}

// execCreateSchedule handles the create_schedule agent tool.
// Creates a recurring or conditional scheduled task with a cron expression.
//
// This is the recurring counterpart to create_reminder. Where create_reminder
// makes a one-shot task, this creates something that fires on a schedule
// (e.g., "every day at 8am").
func execCreateSchedule(argsJSON string, tctx *toolContext) string {
	var args struct {
		Name        string          `json:"name"`
		CronExpr    string          `json:"cron_expr"`
		TaskType    string          `json:"task_type"`
		Payload     json.RawMessage `json:"payload"`
		Priority    string          `json:"priority"`
		MaxRuns     *int            `json:"max_runs"`
		Description string          `json:"description"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	if args.Name == "" {
		return "error: name is required"
	}
	if args.CronExpr == "" {
		return "error: cron_expr is required"
	}
	if args.TaskType == "" {
		return "error: task_type is required"
	}

	// Validate the cron expression before storing it. Better to catch
	// a bad expression now than to have a task in the DB that can never
	// compute its next_run.
	if err := scheduler.ValidateCron(args.CronExpr); err != nil {
		return fmt.Sprintf("error: %v", err)
	}

	// Default priority to "normal" if not specified.
	priority := args.Priority
	if priority == "" {
		priority = "normal"
	}
	// Medication check-ins are always critical — health infrastructure.
	if args.TaskType == "medication_checkin" {
		priority = "critical"
	}

	// Compute the initial next_run from the cron expression.
	loc, _ := time.LoadLocation(tctx.cfg.Scheduler.Timezone)
	if loc == nil {
		loc = time.UTC
	}
	nextRun, err := scheduler.NextRun(args.CronExpr, time.Now(), loc)
	if err != nil {
		return fmt.Sprintf("error computing first run time: %v", err)
	}

	task := &memory.ScheduledTask{
		Name:         &args.Name,
		ScheduleType: "recurring",
		CronExpr:     &args.CronExpr,
		TaskType:     args.TaskType,
		Payload:      args.Payload,
		Enabled:      true,
		NextRun:      &nextRun,
		MaxRuns:      args.MaxRuns,
		Priority:     priority,
		CreatedBy:    "agent",
	}

	id, err := tctx.store.CreateScheduledTask(task)
	if err != nil {
		return fmt.Sprintf("error creating schedule: %v", err)
	}

	nextDisplay := nextRun.In(loc).Format("Mon Jan 2 at 3:04 PM")
	log.Info("agent created schedule",
		"id", id, "name", args.Name, "cron", args.CronExpr,
		"type", args.TaskType, "priority", priority, "next_run", nextRun)

	desc := args.Description
	if desc == "" {
		desc = args.CronExpr
	}

	return fmt.Sprintf("Schedule #%d created: '%s' (%s). Next run: %s. Priority: %s",
		id, args.Name, desc, nextDisplay, priority)
}

// execListSchedules handles the list_schedules agent tool.
// Returns a formatted list of all active scheduled tasks.
func execListSchedules(argsJSON string, tctx *toolContext) string {
	tasks, err := tctx.store.ListActiveTasks()
	if err != nil {
		return fmt.Sprintf("error listing schedules: %v", err)
	}

	if len(tasks) == 0 {
		return "No active scheduled tasks."
	}

	loc, _ := time.LoadLocation(tctx.cfg.Scheduler.Timezone)
	if loc == nil {
		loc = time.UTC
	}

	result := fmt.Sprintf("Active scheduled tasks (%d):\n", len(tasks))
	for _, t := range tasks {
		name := "<unnamed>"
		if t.Name != nil {
			name = *t.Name
		}

		nextStr := "not scheduled"
		if t.NextRun != nil {
			nextStr = t.NextRun.In(loc).Format("Mon Jan 2 at 3:04 PM")
		}

		// Convert cron to human-readable so the agent doesn't have to
		// parse cron syntax (Trinity was misreading "30 9 * * *" as 3pm).
		scheduleStr := ""
		if t.CronExpr != nil && *t.CronExpr != "" {
			scheduleStr = fmt.Sprintf(" | Schedule: %s", scheduler.DescribeCron(*t.CronExpr))
		}

		result += fmt.Sprintf("\n#%d %s\n  Type: %s%s | Priority: %s | Next: %s | Runs: %d",
			t.ID, name, t.TaskType, scheduleStr, t.Priority, nextStr, t.RunCount)
	}

	return result
}

// execUpdateSchedule handles the update_schedule agent tool.
// Pauses or resumes an existing scheduled task.
func execUpdateSchedule(argsJSON string, tctx *toolContext) string {
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

	if err := tctx.store.UpdateScheduledTaskEnabled(args.TaskID, args.Enabled); err != nil {
		return fmt.Sprintf("error updating schedule: %v", err)
	}

	action := "paused"
	if args.Enabled {
		action = "resumed"
	}

	log.Info("agent updated schedule", "id", args.TaskID, "enabled", args.Enabled)
	return fmt.Sprintf("Schedule #%d %s.", args.TaskID, action)
}

// execDeleteSchedule handles the delete_schedule agent tool.
// Permanently removes a scheduled task.
func execDeleteSchedule(argsJSON string, tctx *toolContext) string {
	var args struct {
		TaskID int64 `json:"task_id"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	if args.TaskID == 0 {
		return "error: task_id is required"
	}

	if err := tctx.store.DeleteScheduledTask(args.TaskID); err != nil {
		return fmt.Sprintf("error deleting schedule: %v", err)
	}

	log.Info("agent deleted schedule", "id", args.TaskID)
	return fmt.Sprintf("Schedule #%d deleted.", args.TaskID)
}
