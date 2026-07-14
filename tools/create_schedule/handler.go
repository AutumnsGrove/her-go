package create_schedule

import (
	"bytes"
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
		log.Warnf("create_schedule rejected: invalid task_type %q", a.TaskType)
		return "error: task_type must be one of: worker_briefing, send_message, send_prompt"
	}

	if err := scheduler.ValidateCron(a.CronExpr); err != nil {
		log.Warnf("create_schedule rejected: bad cron_expr %q: %v", a.CronExpr, err)
		return fmt.Sprintf("error: %v", err)
	}

	// Some models double-encode nested object parameters as an escaped JSON
	// string (e.g. "payload": "{\"message\":\"...\"}") instead of a true
	// nested object. json.RawMessage happily captures either shape without
	// erroring, so unwrap a string-encoded payload before validating.
	a.Payload = unwrapStringifiedJSON(a.Payload)

	// Validate payload shape per task type.
	if err := scheduler.ValidatePayload(a.TaskType, a.Payload); err != nil {
		log.Warnf("create_schedule rejected: bad payload for task_type=%s: %v (raw payload: %s)",
			a.TaskType, err, string(a.Payload))
		return fmt.Sprintf("error: %s", err)
	}

	// Resolve timezone for cron evaluation.
	loc := time.UTC
	if ctx.Cfg != nil && ctx.Cfg.Timezone() != "" {
		if parsed, err := time.LoadLocation(ctx.Cfg.Timezone()); err == nil {
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

// unwrapStringifiedJSON detects a JSON payload that is itself a quoted
// string containing escaped JSON (e.g. "\"{\\\"message\\\":\\\"hi\\\"}\"")
// and unwraps it one level. Returns the input unchanged if it isn't a
// string-encoded object.
func unwrapStringifiedJSON(payload json.RawMessage) json.RawMessage {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) < 2 || trimmed[0] != '"' {
		return payload
	}

	var inner string
	if err := json.Unmarshal(trimmed, &inner); err != nil {
		return payload
	}

	innerTrimmed := bytes.TrimSpace([]byte(inner))
	if len(innerTrimmed) == 0 || innerTrimmed[0] != '{' {
		return payload
	}

	return json.RawMessage(innerTrimmed)
}

