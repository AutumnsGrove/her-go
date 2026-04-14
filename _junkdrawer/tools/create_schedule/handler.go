// Package create_schedule implements the create_schedule tool — creates a
// recurring scheduled task with a cron expression.
//
// This is the recurring counterpart to create_reminder. Where create_reminder
// makes a one-shot task, this creates something that fires on a schedule
// (e.g., "every day at 8am", "every weekday at 9am").
package create_schedule

import (
	"encoding/json"
	"fmt"
	"time"

	"her/logger"
	"her/memory"
	"her/scheduler"
	"her/tools"
)

var log = logger.WithPrefix("tools/create_schedule")

func init() {
	tools.Register("create_schedule", Handle)
}

// Handle validates the cron expression, computes the first next_run, and
// creates a recurring ScheduledTask. Medication check-ins are always critical
// priority — health infrastructure should never be damped.
func Handle(argsJSON string, ctx *tools.Context) string {
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
	loc, _ := time.LoadLocation(ctx.Cfg.Scheduler.Timezone)
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

	id, err := ctx.Store.CreateScheduledTask(task)
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
