// Package bot — slash command handlers for general bot commands.
package bot

import (
	"fmt"
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
		"/reflections — view past reflections\n" +
		"/dream — run a full dream cycle now (reflection + persona rewrite)\n\n" +
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

	if err := b.store.DeactivateMemory(factID); err != nil {
		return c.Send(fmt.Sprintf("couldn't forget memory %d: %v", factID, err))
	}

	log.Info("/forget: deactivated memory", "memory_id", factID)
	return c.Send(fmt.Sprintf("Done — forgot memory #%d.", factID))
}

// handleFacts lists all active memories, grouped by subject.
func (b *Bot) handleFacts(c tele.Context) error {
	memories, err := b.store.AllActiveMemories()
	if err != nil {
		return c.Send("couldn't load memories right now, sorry!")
	}

	if len(memories) == 0 {
		return c.Send("No memories saved yet. Keep chatting!")
	}

	var msg strings.Builder
	msg.WriteString("<b>\U0001F9E0 What I Know</b>\n\n")

	currentSubject := ""
	for _, m := range memories {
		if m.Subject != currentSubject {
			currentSubject = m.Subject
			if currentSubject == "user" {
				msg.WriteString("<b>About you:</b>\n")
			} else {
				msg.WriteString("\n<b>About me:</b>\n")
			}
		}
		msg.WriteString(fmt.Sprintf("  #%d [%s] %s\n", m.ID, m.Category, m.Content))
	}

	msg.WriteString("\n<i>Use /forget &lt;id&gt; to remove a memory.</i>")

	// Use pagination — if the fact list exceeds Telegram's 4096-char
	// limit, it'll be split into pages with ◀/▶ navigation buttons.
	return b.sendPaginated(c, msg.String())
}

