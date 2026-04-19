// Zombie bot handlers retired during the mood-tracking redesign.
// Source: bot/callbacks.go, bot/handlers_commands.go, bot/handlers_persona.go
//
// These handlers were alive but inert — the old scheduler runner got junked
// in an earlier cleanup, so the mood check-in, medication check-in, and
// reminder flows had no way to fire. The callback plumbing, the /mood 1-5
// prompt, /schedule list command, /remind command, and snooze helper all
// remained, creating confusion about what actually worked.
//
// They're stashed here verbatim so future work can cherry-pick bits if
// useful (e.g. the agent-pipeline integration for /remind when the
// reminder tool is rebuilt as a scheduler extension).
package zombies

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

// placeholders so the snippets below compile in isolation from bot/
type Bot struct {
	tb       *tele.Bot
	store    *memory.Store
	llm      any
	cfg      any
	agentBusy interface{ Store(bool) }
}

func (b *Bot) SendToChat(chatID int64, text string) error { return nil }
func (b *Bot) baseRunParams() agent.RunParams            { return agent.RunParams{} }
func (b *Bot) handleMessage(c tele.Context) error        { return nil }
func (b *Bot) sendPaginated(c tele.Context, _ string) error {
	_ = c
	return nil
}

var log interface {
	Error(msg string, keyvals ...any)
	Info(msg string, keyvals ...any)
}

// --- Mood Check-in Callback (old 1-5 rating) ---

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

	// Save the mood entry to the database — legacy SaveMoodEntry,
	// now in _junkdrawer/store_mood.go.
	// _, err = b.store.SaveMoodEntry(rating, "", "", "checkin", "scheduled")
	if err != nil {
		log.Error("saving mood from callback", "rating", rating, "err", err)
		return c.Respond(&tele.CallbackResponse{Text: "Something went wrong"})
	}

	log.Info("mood check-in recorded", "rating", rating, "label", moodLabels[rating])

	_ = c.Respond(&tele.CallbackResponse{Text: "Got it!"})
	_ = c.Edit(fmt.Sprintf("mood logged: %s", moodLabels[rating]))

	go b.runMoodFollowUp(c, rating)
	return nil
}

// runMoodFollowUp runs the agent pipeline to generate a contextual
// follow-up after a mood check-in.
func (b *Bot) runMoodFollowUp(c tele.Context, rating int) {
	label := moodLabels[rating]

	prompt := fmt.Sprintf(
		"The user just completed a mood check-in and rated their mood as %d/5 (%s). "+
			"Respond naturally and briefly. For low moods (1-2), gently ask what's going on — "+
			"you have access to their recent context, medication status, and facts. "+
			"For neutral moods (3), a brief acknowledgment is fine. "+
			"For good moods (4-5), share in their positivity briefly. "+
			"Keep it to 1-2 sentences. Don't mention the rating number, just respond to the vibe.",
		rating, label,
	)

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
		_, err := b.store.SaveMemory(
			"User took their evening medication",
			"health", "user",
			0, 5, nil, nil, "", "",
		)
		if err != nil {
			log.Error("saving med memory", "err", err)
		}
		_ = c.Respond(&tele.CallbackResponse{Text: "Logged!"})
		_ = c.Edit("💊 meds taken ✅ — nice job!")

	case "no":
		_, err := b.store.SaveMemory(
			"User did not take their evening medication",
			"health", "user",
			0, 8, nil, nil, "", "",
		)
		if err != nil {
			log.Error("saving med memory", "err", err)
		}
		_ = c.Respond(&tele.CallbackResponse{Text: "Got it"})
		_ = c.Edit("💊 no meds tonight — noted. no pressure 💙")

	case "snooze":
		_ = c.Respond(&tele.CallbackResponse{Text: "I'll ask again in 30 minutes"})
		_ = c.Edit("💊 snoozed — I'll check back in 30 minutes ⏰")

		b.snoozeMedCheckin(30 * time.Minute)

	default:
		_ = c.Respond(&tele.CallbackResponse{Text: "Unknown option"})
	}

	return nil
}

// snoozeMedCheckin creates a one-shot medication check-in task that
// fires after the given delay (legacy scheduled_tasks table).
func (b *Bot) snoozeMedCheckin(delay time.Duration) {
	triggerAt := time.Now().Add(delay)
	_ = triggerAt
	// name := "medication check-in (snoozed)"
	// maxRuns := 1
	// task := &memory.ScheduledTask{...}
	// id, err := b.store.CreateScheduledTask(task)
}

// --- delete_schedule case from executeConfirmedAction ---
//
// case "delete_schedule":
//     var payload struct {
//         TaskID int64 `json:"task_id"`
//     }
//     if err := json.Unmarshal(pending.ActionPayload, &payload); err != nil {
//         return "", fmt.Errorf("bad payload: %v", err)
//     }
//     if payload.TaskID <= 0 {
//         return "", fmt.Errorf("invalid schedule ID: %d", payload.TaskID)
//     }
//     if err := b.store.DeleteScheduledTask(payload.TaskID); err != nil {
//         return "", err
//     }
//     return fmt.Sprintf("Schedule #%d deleted", payload.TaskID), nil
var _ = json.Marshal

// --- /mood command (old 1-5 keyboard) ---

// handleMood sends a mood check-in keyboard on demand.
func (b *Bot) handleMood(c tele.Context) error {
	markup := &tele.ReplyMarkup{}
	row1 := markup.Row(
		markup.Data("😊 Great", "mood", "5"),
		markup.Data("🙂 Good", "mood", "4"),
		markup.Data("😐 Meh", "mood", "3"),
	)
	row2 := markup.Row(
		markup.Data("😔 Rough", "mood", "2"),
		markup.Data("😢 Bad", "mood", "1"),
	)
	markup.Inline(row1, row2)

	return c.Send("how are you feeling right now?", &tele.SendOptions{
		ReplyMarkup: markup,
	})
}

// --- /remind command ---

// handleRemind routes reminder requests through the agent pipeline. The
// agent sees the text as a normal message, recognizes the reminder intent,
// and calls the create_reminder tool with a proper ISO timestamp. Relied
// on the legacy scheduled_tasks table.
func (b *Bot) handleRemind(c tele.Context) error {
	args := strings.TrimSpace(c.Message().Payload)
	if args == "" {
		return c.Send(
			"<b>Usage:</b> /remind <code>&lt;time&gt; &lt;message&gt;</code>\n\n"+
				"<b>Examples:</b>\n"+
				"/remind 3pm call the dentist\n"+
				"/remind tomorrow at 10am take out the trash\n"+
				"/remind in 30 minutes check the oven\n"+
				"/remind next friday review the report",
			&tele.SendOptions{ParseMode: tele.ModeHTML},
		)
	}

	c.Message().Text = "remind me " + args
	return b.handleMessage(c)
}

// --- /schedule command ---

// handleSchedule lists active scheduled tasks or manages them.
// Usage:
//
//	/schedule          — list all active tasks
//	/schedule pause N  — disable task #N
//	/schedule resume N — re-enable task #N
//	/schedule delete N — remove task #N
func (b *Bot) handleSchedule(c tele.Context) error {
	args := strings.TrimSpace(c.Message().Payload)

	if args != "" {
		parts := strings.Fields(args)
		if len(parts) >= 2 {
			action := strings.ToLower(parts[0])
			taskID, err := strconv.ParseInt(parts[1], 10, 64)
			if err != nil {
				return c.Send("Usage: /schedule <pause|resume|delete> <id>")
			}

			switch action {
			case "pause":
				// if err := b.store.UpdateScheduledTaskEnabled(taskID, false); err != nil {
				//     return c.Send(fmt.Sprintf("Couldn't pause task #%d: %v", taskID, err))
				// }
				return c.Send(fmt.Sprintf("⏸ Paused task #%d.", taskID))

			case "resume":
				// if err := b.store.UpdateScheduledTaskEnabled(taskID, true); err != nil {
				//     return c.Send(fmt.Sprintf("Couldn't resume task #%d: %v", taskID, err))
				// }
				return c.Send(fmt.Sprintf("▶️ Resumed task #%d.", taskID))

			case "delete":
				// if err := b.store.DeleteScheduledTask(taskID); err != nil {
				//     return c.Send(fmt.Sprintf("Couldn't delete task #%d: %v", taskID, err))
				// }
				return c.Send(fmt.Sprintf("🗑 Deleted task #%d.", taskID))

			default:
				return c.Send("Unknown action. Try: /schedule pause|resume|delete <id>")
			}
		}
	}

	// Default: list all active tasks.
	// tasks, err := b.store.ListActiveTasks()
	// if err != nil { ... }
	return c.Send("No scheduled tasks. Use /remind to create one!")
}
