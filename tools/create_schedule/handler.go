package create_schedule

import (
	"crypto/sha256"
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

type args struct {
	Name        string          `json:"name"`
	CronExpr    string          `json:"cron_expr"`
	TaskType    string          `json:"task_type"`
	Payload     json.RawMessage `json:"payload"`
	Description string          `json:"description"`
}

var validTaskTypes = map[string]string{
	"worker_briefing": "worker_briefing",
	"send_message":    "send_message",
	"send_prompt":     "send_prompt",
}

// Handle creates a new user-scheduled task. It validates the cron
// expression, auto-injects a 6-char hash prefix on the name for
// collision avoidance, and computes the first fire time.
func Handle(argsJSON string, ctx *tools.Context) string {
	var a args
	if err := json.Unmarshal([]byte(argsJSON), &a); err != nil {
		return "error: invalid arguments"
	}

	if a.Name == "" {
		return "error: name is required"
	}
	if a.CronExpr == "" {
		return "error: cron_expr is required"
	}
	if _, ok := validTaskTypes[a.TaskType]; !ok {
		return "error: task_type must be one of: worker_briefing, send_message, send_prompt"
	}

	if err := scheduler.ValidateCron(a.CronExpr); err != nil {
		return fmt.Sprintf("error: %v", err)
	}

	// Validate payload shape per task type.
	if err := validatePayload(a.TaskType, a.Payload); err != nil {
		return fmt.Sprintf("error: %s", err)
	}

	// Resolve timezone for cron evaluation.
	loc := time.UTC
	if ctx.Cfg != nil && ctx.Cfg.Calendar.DefaultTimezone != "" {
		if parsed, err := time.LoadLocation(ctx.Cfg.Calendar.DefaultTimezone); err == nil {
			loc = parsed
		}
	}

	nextFire, err := scheduler.NextRun(a.CronExpr, time.Now(), loc)
	if err != nil {
		return fmt.Sprintf("error computing next fire time: %v", err)
	}

	// Auto-inject 6-char hash prefix for collision avoidance.
	hashedName := hashName(a.Name)

	// Normalize empty payload to {}.
	payload := a.Payload
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}

	task := &memory.SchedulerTask{
		Kind:     a.TaskType,
		CronExpr: a.CronExpr,
		NextFire: nextFire,
		Payload:  payload,
		Name:     hashedName,
		Enabled:  true,
	}

	id, err := ctx.Store.CreateUserSchedulerTask(task)
	if err != nil {
		log.Error("create_schedule failed", "err", err)
		return fmt.Sprintf("error: %v", err)
	}

	humanCron := scheduler.DescribeCron(a.CronExpr)
	log.Infof("created schedule #%d: %s (%s) — %s", id, hashedName, a.TaskType, humanCron)
	return fmt.Sprintf(
		"Schedule #%d created: %q (%s)\nSchedule: %s (%s)\nNext run: %s",
		id, hashedName, a.TaskType,
		humanCron, a.CronExpr,
		nextFire.In(loc).Format("Mon Jan 2 3:04 PM"),
	)
}

// hashName generates a 6-char hex prefix from the name + current time,
// producing names like "a3f2c1-Morning tech briefing".
func hashName(name string) string {
	h := sha256.Sum256([]byte(name + time.Now().String()))
	return fmt.Sprintf("%x-%s", h[:3], name)
}

func validatePayload(taskType string, payload json.RawMessage) error {
	if len(payload) == 0 || string(payload) == "{}" || string(payload) == "null" {
		if taskType == "send_message" {
			return fmt.Errorf("send_message requires payload with 'message' field")
		}
		if taskType == "send_prompt" {
			return fmt.Errorf("send_prompt requires payload with 'prompt' field")
		}
		return nil
	}

	var p map[string]any
	if err := json.Unmarshal(payload, &p); err != nil {
		return fmt.Errorf("payload must be a JSON object")
	}

	switch taskType {
	case "send_message":
		if _, ok := p["message"]; !ok {
			return fmt.Errorf("send_message payload requires 'message' field")
		}
	case "send_prompt":
		if _, ok := p["prompt"]; !ok {
			return fmt.Errorf("send_prompt payload requires 'prompt' field")
		}
	case "worker_briefing":
		if depth, ok := p["depth"]; ok {
			if d, ok := depth.(string); ok && d != "brief" && d != "deep" {
				return fmt.Errorf("worker_briefing depth must be 'brief' or 'deep'")
			}
		}
	}
	return nil
}
