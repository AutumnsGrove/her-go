// Package bot — slash command handlers for general bot commands.
package bot

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	tele "gopkg.in/telebot.v4"
)

// handleHelp shows all available commands.
func (b *Bot) handleHelp(c tele.Context) error {
	msg := "\U0001F4D6 <b>Commands</b>\n\n" +
		"<b>Conversation</b>\n" +
		"/clear — start a fresh conversation\n" +
		"/compact — summarize older messages to free up context\n\n" +
		"<b>Memory</b>\n" +
		"/facts — list all remembered facts\n" +
		"/forget <code>&lt;id&gt;</code> — forget a specific fact\n\n" +
		"<b>Persona</b>\n" +
		"/persona — view " + b.cfg.Identity.Her + "'s current personality\n" +
		"/persona traits — personality trait scores\n" +
		"/persona rewrite — manually trigger a persona rewrite\n" +
		"/reflect — trigger a reflection\n" +
		"/reflections — view past reflections\n\n" +
		"<b>Reminders</b>\n" +
		"/remind <code>&lt;time&gt; &lt;message&gt;</code> — set a reminder\n" +
		"/schedule — list upcoming reminders\n\n" +
		"<b>Mood &amp; Wellness</b>\n" +
		"/mood — log your current mood (quick buttons)\n\n" +
		"<b>Info</b>\n" +
		"/stats — token usage, cost, and message counts\n" +
		"/status — uptime, models, and service health\n\n" +
		"<b>System</b>\n" +
		"/traces — toggle agent thinking traces in chat\n" +
		"/restart — restart the bot process\n" +
		"/help — this message\n\n" +
		"<b>Features</b>\n" +
		"Send a photo and " + b.cfg.Identity.Her + " will describe what she sees.\n" +
		"Just chat normally — she remembers your conversations."
	return c.Send(msg, &tele.SendOptions{ParseMode: tele.ModeHTML})
}

// handleClear resets the conversation context.
func (b *Bot) handleClear(c tele.Context) error {
	chatID := c.Message().Chat.ID
	key := fmt.Sprintf("%d", chatID)

	newID := fmt.Sprintf("tg_%d_%d", chatID, time.Now().Unix())
	b.conversationIDs.Store(key, newID)

	log.Info("/clear: conversation reset", "chat", chatID, "new_id", newID)
	return c.Send("Context cleared. Fresh start!")
}

// handleStats shows aggregate usage statistics.
func (b *Bot) handleStats(c tele.Context) error {
	stats, err := b.store.GetStats()
	if err != nil {
		return c.Send("couldn't load stats right now, sorry!")
	}

	var cmdSection string
	if stats.TotalCommands > 0 {
		cmdSection = fmt.Sprintf("\n\n<b>Commands:</b> %d total\n", stats.TotalCommands)
		for _, cc := range stats.CommandCounts {
			cmdSection += fmt.Sprintf("  %s: %d\n", cc.Command, cc.Count)
		}
	}

	msg := fmt.Sprintf(
		"<b>\U0001F4CA Stats</b>\n\n"+
			"<b>Messages:</b> %d total (%d you, %d me)\n"+
			"<b>Active days:</b> %d\n\n"+
			"<b>Memory:</b> %d facts (%d about you, %d about me)\n\n"+
			"<b>Tokens:</b> %s total\n"+
			"  Chat: %s ($%.4f)\n"+
			"  Agent: %s ($%.4f)\n"+
			"<b>Total cost:</b> $%.4f\n"+
			"<b>Avg latency:</b> %dms%s",
		stats.TotalMessages, stats.UserMessages, stats.MiraMessages,
		stats.ConversationDays,
		stats.TotalFacts, stats.UserFacts, stats.SelfFacts,
		formatTokens(stats.TotalTokens),
		formatTokens(stats.ChatTokens), stats.ChatCostUSD,
		formatTokens(stats.AgentTokens), stats.AgentCostUSD,
		stats.TotalCostUSD,
		stats.AvgLatencyMs,
		cmdSection,
	)

	return c.Send(msg, &tele.SendOptions{ParseMode: tele.ModeHTML})
}

// handleForget deactivates a fact by ID.
func (b *Bot) handleForget(c tele.Context) error {
	args := strings.TrimSpace(c.Message().Payload)

	if args == "" {
		return b.handleFacts(c)
	}

	var factID int64
	if _, err := fmt.Sscanf(args, "%d", &factID); err != nil {
		return c.Send("usage: /forget <fact_id>\n\nUse /facts to see all active facts with their IDs.")
	}

	if err := b.store.DeactivateFact(factID); err != nil {
		return c.Send(fmt.Sprintf("couldn't forget fact %d: %v", factID, err))
	}

	log.Info("/forget: deactivated fact", "fact_id", factID)
	return c.Send(fmt.Sprintf("Done — forgot fact #%d.", factID))
}

// handleFacts lists all active facts, grouped by subject.
func (b *Bot) handleFacts(c tele.Context) error {
	facts, err := b.store.AllActiveFacts()
	if err != nil {
		return c.Send("couldn't load facts right now, sorry!")
	}

	if len(facts) == 0 {
		return c.Send("No facts saved yet. Keep chatting!")
	}

	var msg strings.Builder
	msg.WriteString("<b>\U0001F9E0 What I Know</b>\n\n")

	currentSubject := ""
	for _, f := range facts {
		if f.Subject != currentSubject {
			currentSubject = f.Subject
			if currentSubject == "user" {
				msg.WriteString("<b>About you:</b>\n")
			} else {
				msg.WriteString("\n<b>About me:</b>\n")
			}
		}
		msg.WriteString(fmt.Sprintf("  #%d [%s] %s\n", f.ID, f.Category, f.Fact))
	}

	msg.WriteString("\n<i>Use /forget &lt;id&gt; to remove a fact.</i>")

	// Use pagination — if the fact list exceeds Telegram's 4096-char
	// limit, it'll be split into pages with ◀/▶ navigation buttons.
	return b.sendPaginated(c, msg.String())
}

// handleRemind routes reminder requests through the agent pipeline.
// Instead of trying to parse natural language time ourselves (which is
// brittle), we let the LLM do what it's good at — understanding
// "in 2 mins", "tomorrow at 3pm", "next friday morning", etc.
//
// The agent sees the text as a normal message, recognizes the reminder
// intent, and calls the create_reminder tool with a proper ISO timestamp.
// This means /remind is really just a convenience shortcut — the user
// could also just say "remind me to call the dentist at 3pm" in normal
// conversation and the agent would do the same thing.
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

	// Rewrite the command as a natural message and feed it through
	// the agent pipeline. The agent will parse the time, call
	// create_reminder, and reply with a confirmation.
	c.Message().Text = "remind me " + args
	return b.handleMessage(c)
}

// handleSchedule lists active scheduled tasks or manages them.
// Usage:
//
//	/schedule          — list all active tasks
//	/schedule pause N  — disable task #N
//	/schedule resume N — re-enable task #N
//	/schedule delete N — remove task #N
func (b *Bot) handleSchedule(c tele.Context) error {
	args := strings.TrimSpace(c.Message().Payload)

	// Sub-commands: pause, resume, delete.
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
				if err := b.store.UpdateScheduledTaskEnabled(taskID, false); err != nil {
					return c.Send(fmt.Sprintf("Couldn't pause task #%d: %v", taskID, err))
				}
				return c.Send(fmt.Sprintf("⏸ Paused task #%d.", taskID))

			case "resume":
				if err := b.store.UpdateScheduledTaskEnabled(taskID, true); err != nil {
					return c.Send(fmt.Sprintf("Couldn't resume task #%d: %v", taskID, err))
				}
				return c.Send(fmt.Sprintf("▶️ Resumed task #%d.", taskID))

			case "delete":
				if err := b.store.DeleteScheduledTask(taskID); err != nil {
					return c.Send(fmt.Sprintf("Couldn't delete task #%d: %v", taskID, err))
				}
				return c.Send(fmt.Sprintf("🗑 Deleted task #%d.", taskID))

			default:
				return c.Send("Unknown action. Try: /schedule pause|resume|delete <id>")
			}
		}
	}

	// Default: list all active tasks.
	tasks, err := b.store.ListActiveTasks()
	if err != nil {
		log.Error("/schedule: listing tasks", "err", err)
		return c.Send("Couldn't load scheduled tasks right now.")
	}

	if len(tasks) == 0 {
		return c.Send("No scheduled tasks. Use /remind to create one!")
	}

	loc := time.Local

	var sb strings.Builder
	sb.WriteString("<b>📋 Scheduled Tasks</b>\n\n")

	for _, t := range tasks {
		name := "unnamed"
		if t.Name != nil {
			name = *t.Name
		}

		nextRun := "—"
		if t.NextRun != nil {
			nextRun = t.NextRun.In(loc).Format("Mon Jan 2 at 3:04 PM")
		}

		sb.WriteString(fmt.Sprintf(
			"<b>#%d</b> %s\n  ⏰ %s | type: %s\n\n",
			t.ID, name, nextRun, t.TaskType,
		))
	}

	sb.WriteString("<i>/schedule pause|resume|delete &lt;id&gt;</i>")

	return c.Send(sb.String(), &tele.SendOptions{ParseMode: tele.ModeHTML})
}
