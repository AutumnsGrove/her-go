// Package create_reminder implements the create_reminder tool — creates a
// one-shot reminder that fires at a specific time.
//
// The agent has already parsed the user's natural language time expression
// into an ISO 8601 timestamp — we validate it, apply the configured timezone
// if no timezone was included, and create the scheduled task.
package create_reminder

import (
	"encoding/json"
	"fmt"
	"time"

	"her/logger"
	"her/memory"
	"her/tools"
)

var log = logger.WithPrefix("tools/create_reminder")

func init() {
	tools.Register("create_reminder", Handle)
}

// Handle validates the ISO 8601 timestamp, applies timezone if missing,
// checks it's in the future, and creates a once-type ScheduledTask.
//
// Go's time.Parse uses a reference time instead of format codes like Python's
// strftime. The layout "2006-01-02T15:04:05" is Go's magic reference date
// (Jan 2, 2006 at 3:04:05 PM). Every digit in the layout is unique so Go
// can tell year from month from day. Yes, it's weird — everyone says that.
func Handle(argsJSON string, ctx *tools.Context) string {
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

	// Parse the ISO 8601 timestamp. Try without timezone first (bare
	// "2026-03-22T15:00:00"), then with timezone offset (RFC3339).
	triggerAt, err := time.Parse("2006-01-02T15:04:05", args.TriggerAt)
	if err != nil {
		triggerAt, err = time.Parse(time.RFC3339, args.TriggerAt)
		if err != nil {
			return fmt.Sprintf("error: couldn't parse trigger_at '%s' — expected ISO 8601 format like '2026-03-22T15:00:00'", args.TriggerAt)
		}
	}

	// Apply the configured timezone if the timestamp didn't include one.
	// A bare "2026-03-22T15:00:00" should mean 3pm in the USER's timezone,
	// not UTC. The 'Z' suffix check detects explicit UTC timestamps.
	if triggerAt.Location() == time.UTC && args.TriggerAt[len(args.TriggerAt)-1] != 'Z' {
		loc, locErr := time.LoadLocation(ctx.Cfg.Scheduler.Timezone)
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

	// Build the task name (truncated to 60 chars for readability in list_schedules).
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
		Priority:        "critical", // user-requested reminders always fire, bypassing damping
		CreatedBy:       "agent",
		SourceMessageID: &ctx.TriggerMsgID,
	}

	id, err := ctx.Store.CreateScheduledTask(task)
	if err != nil {
		return fmt.Sprintf("error creating reminder: %v", err)
	}

	timeDisplay := triggerAt.Format("Mon Jan 2 at 3:04 PM")
	log.Info("agent created reminder", "id", id, "trigger_at", triggerAt, "message", args.Message)

	return fmt.Sprintf("Reminder #%d created: '%s' at %s", id, args.Message, timeDisplay)
}
