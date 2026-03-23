package agent

import (
	"encoding/json"
	"fmt"
	"time"

	"her/memory"
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
