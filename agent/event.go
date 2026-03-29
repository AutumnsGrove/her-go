package agent

import "time"

// AgentEventType identifies what kind of event triggered an agent run.
// Each event type carries different fields in AgentEvent.
//
// This is a Go "enum" — a typed int with named constants. iota + 1
// means the zero value (0) doesn't match any event type, which helps
// catch uninitialized events. Same pattern as TrustLevel in loader/.
type AgentEventType int

const (
	// EventSchedulerFired means a scheduled task (morning briefing,
	// follow-up, etc.) needs to run through the agent pipeline.
	// Uses: Prompt, TaskName.
	EventSchedulerFired AgentEventType = iota + 1

	// EventSkillFailed means a skill execution failed (timeout, crash,
	// non-zero exit). The agent can decide whether to notify the user,
	// retry, or take corrective action.
	// Uses: SkillName, Error.
	EventSkillFailed

	// EventCodingComplete will fire when the coding agent finishes
	// editing or creating a skill. Not yet implemented — waiting for
	// the delegate_coding tool.
	// Will use: SkillName, Result, Success.
	EventCodingComplete

	// EventDDLDetected fires when a skill modifies its sidecar database
	// schema (CREATE TABLE, ALTER TABLE, DROP TABLE). The agent decides
	// the appropriate response: log silently, mention to Autumn, or
	// quarantine the skill.
	// Uses: SkillName, DDLStatement.
	EventDDLDetected
)

// String implements fmt.Stringer for readable logging.
func (t AgentEventType) String() string {
	switch t {
	case EventSchedulerFired:
		return "scheduler-fired"
	case EventSkillFailed:
		return "skill-failed"
	case EventCodingComplete:
		return "coding-complete"
	case EventDDLDetected:
		return "ddl-detected"
	default:
		return "unknown"
	}
}

// AgentEvent is something that triggers an agent run without a user message.
//
// The bot's event consumption loop receives these from a channel and builds
// the appropriate RunParams for each type. Different event types use
// different fields — check the constants above for which fields each uses.
//
// This is the generalized version of the scheduler's old agentFn callback.
// Instead of each trigger source having its own callback, they all emit
// AgentEvents into a shared channel.
type AgentEvent struct {
	Type AgentEventType

	// --- EventSchedulerFired fields ---
	Prompt   string // the prompt text to run through the agent
	TaskName string // task name for logging ("morning briefing", etc.)

	// --- EventSkillFailed fields ---
	SkillName string // which skill failed or modified its schema
	Error     string // error description

	// --- EventDDLDetected fields ---
	DDLStatement string // the DDL statement that was executed (CREATE TABLE, etc.)

	// --- Common ---
	Timestamp time.Time
}
