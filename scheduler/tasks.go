// tasks.go — Task type executors.
//
// Each supported task_type in the scheduled_tasks table has a
// corresponding execute function here. The scheduler's executeTask()
// method dispatches to these based on task.TaskType.
//
// Current task types:
//   - "send_message"       — send a plain text message to the user
//   - "run_prompt"         — run a prompt through the full agent pipeline
//   - "mood_checkin"       — send mood check-in with inline keyboard
//   - "medication_checkin" — send medication check-in with inline keyboard
//
// Future types:
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
	if s.agentEventFn == nil {
		log.Error("run_prompt task but no agentEventFn configured — is the scheduler fully wired?",
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

	log.Info("emitting scheduled prompt event", "id", task.ID, "name", name,
		"prompt_len", len(payload.Prompt))

	// Fire-and-forget: emit an agent event. The bot's event consumer
	// handles the actual agent.Run() call asynchronously. The old
	// agentFn blocked here; now the scheduler continues immediately.
	s.agentEventFn(name, payload.Prompt)
}

// executeMoodCheckin handles the "mood_checkin" task type.
// Sends a message with emoji buttons for the user to rate their mood.
// The actual mood logging happens in the bot's callback handler when
// the user clicks a button — the scheduler just sends the prompt.
//
// Button layout: two rows (3 + 2) to fit comfortably on phone screens.
func (s *Scheduler) executeMoodCheckin(task memory.ScheduledTask) {
	if s.sendKeyboardFn == nil {
		log.Error("mood_checkin task but no sendKeyboardFn configured", "id", task.ID)
		return
	}

	var payload struct {
		Style    string `json:"style"`     // "gentle" or "direct"
		FollowUp bool   `json:"follow_up"` // whether low-mood triggers follow-up
	}
	if err := json.Unmarshal(task.Payload, &payload); err != nil {
		log.Error("parsing mood_checkin payload", "id", task.ID, "err", err)
		return
	}

	// The message text varies by style. Gentle is the default — warm
	// and casual. Direct is more straightforward.
	text := "hey, how are you feeling right now?"
	if payload.Style == "direct" {
		text = "mood check — how's it going?"
	}

	keyboard := InlineKeyboard{
		// Row 1: positive + neutral
		{
			{Text: "😊 Great", Action: "mood", Value: "5"},
			{Text: "🙂 Good", Action: "mood", Value: "4"},
			{Text: "😐 Meh", Action: "mood", Value: "3"},
		},
		// Row 2: negative (fewer buttons = more thumb room)
		{
			{Text: "😔 Rough", Action: "mood", Value: "2"},
			{Text: "😢 Bad", Action: "mood", Value: "1"},
		},
	}

	if err := s.sendKeyboardFn(KeyboardMessage{Text: text, Keyboard: keyboard}); err != nil {
		log.Error("sending mood check-in", "id", task.ID, "err", err)
	}
}

// executeMedicationCheckin handles the "medication_checkin" task type.
// Sends a message with Yes/No/Snooze buttons for medication tracking.
// Like mood check-ins, the actual logging happens in the bot's callback
// handler — the scheduler just delivers the prompt.
func (s *Scheduler) executeMedicationCheckin(task memory.ScheduledTask) {
	if s.sendKeyboardFn == nil {
		log.Error("medication_checkin task but no sendKeyboardFn configured", "id", task.ID)
		return
	}

	var payload struct {
		TimeOfDay string `json:"time_of_day"` // "morning" or "evening"
	}
	if err := json.Unmarshal(task.Payload, &payload); err != nil {
		log.Error("parsing medication_checkin payload", "id", task.ID, "err", err)
		return
	}

	text := "💊 hey, did you take your meds tonight?"
	if payload.TimeOfDay == "morning" {
		text = "💊 good morning! did you take your meds?"
	}

	keyboard := InlineKeyboard{
		{
			{Text: "✅ Yes", Action: "med", Value: "yes"},
			{Text: "❌ No", Action: "med", Value: "no"},
			{Text: "⏰ Snooze 30m", Action: "med", Value: "snooze"},
		},
	}

	if err := s.sendKeyboardFn(KeyboardMessage{Text: text, Keyboard: keyboard}); err != nil {
		log.Error("sending medication check-in", "id", task.ID, "err", err)
	}
}
