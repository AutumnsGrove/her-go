// callbacks.go — Inline keyboard callback handling.
//
// Routes callback queries when users click inline buttons (pagination,
// confirmations, etc.). Each button's Unique field maps to a handler
// registered in registerCallbackHandlers.
package bot

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"her/agent"
	"her/memory"

	tele "gopkg.in/telebot.v4"
)

// registerCallbackHandlers sets up handlers for inline button callbacks.
// Each Action value used in Button types needs a corresponding handler
// registered here. Adding a new button type is a one-liner.
//
// Called from New() during bot initialization.
func (b *Bot) registerCallbackHandlers() {
	// Mood check-in buttons (Action: "mood")
	b.tb.Handle(&tele.InlineButton{Unique: "mood"}, b.handleMoodCallback)

	// Medication check-in buttons (Action: "med")
	b.tb.Handle(&tele.InlineButton{Unique: "med"}, b.handleMedCallback)

	// Agent confirmation buttons (Action: "confirm") — handles Yes/No
	// for destructive actions triggered by the reply_confirm tool.
	b.tb.Handle(&tele.InlineButton{Unique: "confirm"}, b.handleConfirmCallback)

	// Pagination buttons (Action: "page") — handles ◀/▶ navigation
	// for any command that produces output longer than 4096 chars.
	b.tb.Handle(&tele.InlineButton{Unique: "page"}, b.handlePageCallback)
}

// --- Mood Check-in Callback ---

// moodLabels maps rating numbers to their display labels.
var moodLabels = map[int]string{
	1: "😢 Bad",
	2: "😔 Rough",
	3: "😐 Meh",
	4: "🙂 Good",
	5: "😊 Great",
}

// handleMoodCallback fires when the user clicks a mood check-in button.
// It saves the mood entry, edits the message to show the selection,
// and runs the agent for a dynamic, contextual follow-up.
func (b *Bot) handleMoodCallback(c tele.Context) error {
	data := strings.TrimSpace(c.Callback().Data)
	rating, err := strconv.Atoi(data)
	if err != nil || rating < 1 || rating > 5 {
		return c.Respond(&tele.CallbackResponse{Text: "Invalid rating"})
	}

	// Save the mood entry to the database.
	_, err = b.store.SaveMoodEntry(rating, "", "", "checkin", "scheduled")
	if err != nil {
		log.Error("saving mood from callback", "rating", rating, "err", err)
		return c.Respond(&tele.CallbackResponse{Text: "Something went wrong"})
	}

	log.Info("mood check-in recorded", "rating", rating, "label", moodLabels[rating])

	// Acknowledge the click (removes the loading spinner on the button).
	_ = c.Respond(&tele.CallbackResponse{Text: "Got it!"})

	// Edit the original message to show the selection and remove the
	// keyboard. Once you've clicked, the buttons disappear — replaced
	// by a confirmation of what you chose.
	_ = c.Edit(fmt.Sprintf("mood logged: %s", moodLabels[rating]))

	// Run the agent for a dynamic follow-up. Instead of a canned
	// "What's going on?" every time, the agent draws on mood trends,
	// medication history, recent facts, and everything else to craft
	// a response that actually feels alive.
	go b.runMoodFollowUp(c, rating)

	return nil
}

// runMoodFollowUp runs the agent pipeline to generate a contextual
// follow-up after a mood check-in. Runs in a goroutine so it doesn't
// block the callback response.
func (b *Bot) runMoodFollowUp(c tele.Context, rating int) {
	label := moodLabels[rating]

	// Build a prompt that gives the agent full context about the
	// mood rating. The agent decides how to respond — warm
	// acknowledgment for good moods, gentle check-in for rough ones.
	prompt := fmt.Sprintf(
		"The user just completed a mood check-in and rated their mood as %d/5 (%s). "+
			"Respond naturally and briefly. For low moods (1-2), gently ask what's going on — "+
			"you have access to their recent context, medication status, and facts. "+
			"For neutral moods (3), a brief acknowledgment is fine. "+
			"For good moods (4-5), share in their positivity briefly. "+
			"Keep it to 1-2 sentences. Don't mention the rating number, just respond to the vibe.",
		rating, label,
	)

	// Build a minimal agent run — no placeholder, typing, or traces.
	// Event-triggered runs just send new messages to the chat.
	chatID := c.Chat().ID
	sendFn := func(text string) error {
		return b.SendToChat(chatID, text)
	}

	params := b.baseRunParams()
	params.ScrubbedUserMessage = prompt
	params.ConversationID = "mood-checkin"
	params.StatusCallback = sendFn

	b.agentBusy.Store(true)
	result, err := agent.Run(params)
	b.agentBusy.Store(false)
	if err != nil {
		log.Error("mood follow-up agent error", "err", err)
		return
	}

	log.Info("mood follow-up sent", "rating", rating, "reply_len", len(result.ReplyText))
}

// --- Medication Check-in Callback ---

// handleMedCallback fires when the user clicks a medication check-in button.
// Handles three actions: yes (took meds), no (didn't), snooze (ask again later).
func (b *Bot) handleMedCallback(c tele.Context) error {
	data := strings.TrimSpace(c.Callback().Data)

	switch data {
	case "yes":
		// Log as a fact — medication adherence tracked through the
		// existing fact system rather than a separate table.
		_, err := b.store.SaveFact(
			"User took their evening medication",
			"health", "user",
			0, // no source message
			5, // default importance
			nil, // no tag embedding needed
			nil, // no text embedding needed
			"",  // no tags
			"",  // no context
		)
		if err != nil {
			log.Error("saving med fact", "err", err)
		}
		_ = c.Respond(&tele.CallbackResponse{Text: "Logged!"})
		_ = c.Edit("💊 meds taken ✅ — nice job!")

	case "no":
		_, err := b.store.SaveFact(
			"User did not take their evening medication",
			"health", "user",
			0, 8, nil, nil, "", "",
		)
		if err != nil {
			log.Error("saving med fact", "err", err)
		}
		_ = c.Respond(&tele.CallbackResponse{Text: "Got it"})
		// Gentle — no judgment. The bot is tracking, not nagging.
		_ = c.Edit("💊 no meds tonight — noted. no pressure 💙")

	case "snooze":
		_ = c.Respond(&tele.CallbackResponse{Text: "I'll ask again in 30 minutes"})
		_ = c.Edit("💊 snoozed — I'll check back in 30 minutes ⏰")

		// Create a one-shot medication check-in 30 minutes from now.
		// The scheduler picks it up on its next tick. Priority is
		// critical because medication reminders always fire.
		b.snoozeMedCheckin(30 * time.Minute)

	default:
		_ = c.Respond(&tele.CallbackResponse{Text: "Unknown option"})
	}

	return nil
}

// snoozeMedCheckin creates a one-shot medication check-in task that
// fires after the given delay. Used when the user taps "Snooze" on
// a medication check-in.
func (b *Bot) snoozeMedCheckin(delay time.Duration) {
	triggerAt := time.Now().Add(delay)
	name := "medication check-in (snoozed)"

	maxRuns := 1
	task := &memory.ScheduledTask{
		Name:         &name,
		ScheduleType: "once",
		TriggerAt:    &triggerAt,
		TaskType:     "medication_checkin",
		Payload:      []byte(`{"time_of_day":"evening"}`),
		Enabled:      true,
		NextRun:      &triggerAt,
		MaxRuns:      &maxRuns,
		Priority:     "critical", // medication always fires
		CreatedBy:    "system",
	}

	id, err := b.store.CreateScheduledTask(task)
	if err != nil {
		log.Error("creating snoozed med check-in", "err", err)
		return
	}

	log.Info("snoozed medication check-in", "id", id,
		"trigger_at", triggerAt.Format("3:04 PM"))
}

// --- Agent Confirmation Callback ---

// handleConfirmCallback fires when the user clicks Yes or No on a
// confirmation prompt sent by the reply_confirm agent tool. It looks up
// the pending action by Telegram message ID, executes or cancels it,
// and edits the message to show the result.
//
// This is the same pattern as handleMoodCallback — read the button's
// data, take action, edit the message — but agent-driven instead of
// scheduler-driven. The key difference is that we look up a stored
// pending action instead of knowing what to do from the button value.
func (b *Bot) handleConfirmCallback(c tele.Context) error {
	data := strings.TrimSpace(c.Callback().Data)

	// c.Callback().Message is the message that has the inline buttons.
	// Its ID is what we used to key the pending_confirmations table.
	msgID := int64(c.Callback().Message.ID)

	// Look up the pending confirmation by Telegram message ID.
	pending, err := b.store.GetPendingConfirmation(msgID)
	if err != nil {
		log.Error("looking up pending confirmation", "msg_id", msgID, "err", err)
		return c.Respond(&tele.CallbackResponse{Text: "Something went wrong"})
	}
	if pending == nil {
		// Already resolved (double-click) or expired (>1 hour old).
		return c.Respond(&tele.CallbackResponse{Text: "This confirmation has expired"})
	}

	// Get the conversation ID so we can record the outcome in history.
	// Without this, the agent has no idea the user responded to the
	// confirmation — it sees "I sent you a confirmation" in its own
	// reply but never sees the resolution, leading to confused follow-ups
	// like "did you see the confirmation I sent?"
	chatID := c.Callback().Message.Chat.ID
	convID := b.getConversationID(chatID)

	if data == "no" {
		// User cancelled — mark as cancelled and update the message.
		_ = b.store.ResolvePendingConfirmation(pending.ID, "cancelled")
		_ = c.Respond(&tele.CallbackResponse{Text: "Cancelled"})
		_ = c.Edit("❌ " + pending.Description + " — cancelled")
		log.Info("confirmation cancelled", "id", pending.ID, "action", pending.ActionType)

		// Record in conversation history so the agent knows.
		note := fmt.Sprintf("[User cancelled: %s]", pending.Description)
		b.store.SaveMessage("user", note, note, convID)
		return nil
	}

	// data == "yes" — execute the pending action.
	result, err := b.executeConfirmedAction(pending)
	if err != nil {
		_ = b.store.ResolvePendingConfirmation(pending.ID, "error")
		_ = c.Respond(&tele.CallbackResponse{Text: "Something went wrong"})
		_ = c.Edit("⚠️ " + pending.Description + " — failed: " + err.Error())
		log.Error("executing confirmed action", "id", pending.ID, "action", pending.ActionType, "err", err)
		return nil
	}

	_ = b.store.ResolvePendingConfirmation(pending.ID, "confirmed")
	_ = c.Respond(&tele.CallbackResponse{Text: "Done!"})
	_ = c.Edit("✅ " + result)
	log.Info("confirmation executed", "id", pending.ID, "action", pending.ActionType, "result", result)

	// Record the confirmed action in conversation history so the agent
	// knows the user already responded. Without this, the next turn's
	// context has no record of the resolution — the agent sees its own
	// "I sent you a confirmation" but not the user's click.
	note := fmt.Sprintf("[User confirmed: %s]", result)
	b.store.SaveMessage("user", note, note, convID)
	return nil
}

// executeConfirmedAction runs the actual destructive action described
// by a pending confirmation. Each action_type maps to a specific store
// method. Adding new confirmable actions means adding a case here AND
// in agent/confirm.go's validConfirmActions map.
//
// This runs directly against b.store — no agent loop or toolContext
// needed. That's the beauty of storing the action in the DB: the
// callback handler is self-contained.
func (b *Bot) executeConfirmedAction(pending *memory.PendingConfirmation) (string, error) {
	switch pending.ActionType {
	case "delete_expense":
		// Supports both single ID and multiple IDs:
		//   {"id": 42}       → delete one expense
		//   {"ids": [1,2,3]} → delete several expenses in one confirmation
		var payload struct {
			ID  int64   `json:"id"`
			IDs []int64 `json:"ids"`
		}
		if err := json.Unmarshal(pending.ActionPayload, &payload); err != nil {
			return "", fmt.Errorf("bad payload: %v", err)
		}

		// Normalize: if single ID was provided, treat it as a one-element list.
		ids := payload.IDs
		if payload.ID > 0 && len(ids) == 0 {
			ids = []int64{payload.ID}
		}
		if len(ids) == 0 {
			return "", fmt.Errorf("no expense IDs provided")
		}

		var deleted int
		for _, id := range ids {
			if id <= 0 {
				continue
			}
			if err := b.store.DeleteExpense(id); err != nil {
				log.Error("deleting expense in batch", "id", id, "err", err)
				continue // best-effort — delete what we can
			}
			deleted++
		}
		if deleted == 0 {
			return "", fmt.Errorf("no expenses were deleted")
		}
		if deleted == 1 {
			return fmt.Sprintf("Expense #%d deleted", ids[0]), nil
		}
		return fmt.Sprintf("%d expenses deleted", deleted), nil

	case "remove_fact":
		var payload struct {
			FactID int64 `json:"fact_id"`
		}
		if err := json.Unmarshal(pending.ActionPayload, &payload); err != nil {
			return "", fmt.Errorf("bad payload: %v", err)
		}
		if payload.FactID <= 0 {
			return "", fmt.Errorf("invalid fact ID: %d", payload.FactID)
		}
		if err := b.store.DeactivateFact(payload.FactID); err != nil {
			return "", err
		}
		return fmt.Sprintf("Fact #%d removed", payload.FactID), nil

	case "delete_schedule":
		var payload struct {
			TaskID int64 `json:"task_id"`
		}
		if err := json.Unmarshal(pending.ActionPayload, &payload); err != nil {
			return "", fmt.Errorf("bad payload: %v", err)
		}
		if payload.TaskID <= 0 {
			return "", fmt.Errorf("invalid schedule ID: %d", payload.TaskID)
		}
		if err := b.store.DeleteScheduledTask(payload.TaskID); err != nil {
			return "", err
		}
		return fmt.Sprintf("Schedule #%d deleted", payload.TaskID), nil

	default:
		return "", fmt.Errorf("unknown action type: %s", pending.ActionType)
	}
}
