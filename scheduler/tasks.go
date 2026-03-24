// tasks.go — Task type executors.
//
// Each supported task_type in the scheduled_tasks table has a
// corresponding execute function here. The scheduler's executeTask()
// method dispatches to these based on task.TaskType.
//
// Current task types:
//   - "send_message" — send a plain text message to the user
//   - "run_prompt"   — run a prompt through the full agent pipeline
//
// Future types (v0.6 inline keyboards phase):
//   - "mood_checkin"       — send mood check-in with inline keyboard
//   - "medication_checkin" — send medication check-in
//   - "run_extraction"     — trigger fact extraction
//   - "run_journal"        — generate auto-journal entry
package scheduler

import (
	"encoding/json"
	"fmt"

	"her/memory"
)

// executeSendMessage handles the "send_message" task type.
// The payload is a JSON object with a "message" field.
func (s *Scheduler) executeSendMessage(task memory.ScheduledTask) {
	// Parse the payload to extract the message text.
	// json.RawMessage is already a []byte of JSON — we just need to
	// unmarshal it into a Go struct. Like json.loads() in Python.
	var payload struct {
		Message string `json:"message"`
	}
	if err := json.Unmarshal(task.Payload, &payload); err != nil {
		log.Error("parsing send_message payload", "id", task.ID, "err", err)
		return
	}

	if payload.Message == "" {
		log.Warn("send_message task has empty message", "id", task.ID)
		return
	}

	// Format the reminder message with a label.
	name := "Reminder"
	if task.Name != nil && *task.Name != "" {
		name = *task.Name
	}
	text := fmt.Sprintf("⏰ %s\n\n%s", name, payload.Message)

	if err := s.sendFn(text); err != nil {
		log.Error("sending scheduled message", "id", task.ID, "err", err)
	}
}

// executeRunPrompt handles the "run_prompt" task type.
//
// This is the most powerful task type — it sends the prompt through
// the full agent pipeline via the agentFn callback. The agent can use
// all its tools (search, memory, facts, etc.) and generates a natural
// response that gets delivered to the user.
//
// This is the "escape hatch" described in the spec: anything that can
// be described as a prompt can be scheduled. Morning briefings,
// follow-ups, journal entries — they're all just prompts with different
// instructions.
func (s *Scheduler) executeRunPrompt(task memory.ScheduledTask) {
	if s.agentFn == nil {
		log.Error("run_prompt task but no agentFn configured — is the scheduler fully wired?",
			"id", task.ID)
		return
	}

	var payload struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(task.Payload, &payload); err != nil {
		log.Error("parsing run_prompt payload", "id", task.ID, "err", err)
		return
	}

	if payload.Prompt == "" {
		log.Warn("run_prompt task has empty prompt", "id", task.ID)
		return
	}

	name := "<unnamed>"
	if task.Name != nil {
		name = *task.Name
	}

	log.Info("running scheduled prompt", "id", task.ID, "name", name,
		"prompt_len", len(payload.Prompt))

	// The agentFn runs the prompt through the full agent pipeline.
	// The agent's reply tool delivers the message to the user via
	// the StatusCallback (which for scheduled runs sends a new
	// Telegram message rather than editing a placeholder).
	replyText, err := s.agentFn(payload.Prompt)
	if err != nil {
		log.Error("agent pipeline error for run_prompt", "id", task.ID, "err", err)
		return
	}

	log.Info("run_prompt completed", "id", task.ID, "name", name,
		"reply_len", len(replyText))
}
