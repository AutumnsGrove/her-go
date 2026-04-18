// Package bot — persona, reflection, and mood handlers.
package bot

import (
	"fmt"
	"os"
	"strings"
	"time"

	"her/persona"

	tele "gopkg.in/telebot.v4"
)

// handleMood sends a mood check-in keyboard on demand — the same one
// the scheduler sends on a cron. Lets you log your mood any time with
// /mood instead of waiting for the scheduled check-in.
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

// handleReflect manually triggers a reflection.
func (b *Bot) handleReflect(c tele.Context) error {
	_ = c.Notify(tele.Typing)

	recent, err := b.store.GlobalRecentMessages(10)
	if err != nil || len(recent) < 2 {
		return c.Send("Not enough conversation history to reflect on yet. Keep chatting!")
	}

	memories, _ := b.store.RecentMemories("user", 10)
	selfMemories, _ := b.store.RecentMemories("self", 10)

	var factStrings []string
	for _, m := range memories {
		factStrings = append(factStrings, m.Content)
	}
	for _, m := range selfMemories {
		if m.Category != "reflection" {
			factStrings = append(factStrings, "(self) "+m.Content)
		}
	}

	if len(factStrings) == 0 {
		return c.Send("I don't have enough memories to reflect on yet. Let's keep talking!")
	}

	var lastUser, lastBot string
	for i := len(recent) - 1; i >= 0; i-- {
		if recent[i].Role == "user" && lastUser == "" {
			lastUser = recent[i].ContentRaw
		}
		if recent[i].Role == "assistant" && lastBot == "" {
			lastBot = recent[i].ContentRaw
		}
		if lastUser != "" && lastBot != "" {
			break
		}
	}

	err = persona.Reflect(b.llm, b.store, lastUser, lastBot, factStrings, b.cfg.Identity.Her, b.cfg.Identity.User)
	if err != nil {
		log.Error("manual reflection", "err", err)
		return c.Send("I tried to reflect but something went wrong. Try again?")
	}

	reflections, _ := b.store.ReflectionsSince(time.Now().Add(-10 * time.Second))
	if len(reflections) > 0 {
		return c.Send(fmt.Sprintf("\U0001F4AD <b>Reflection</b>\n\n<i>%s</i>", reflections[len(reflections)-1].Content),
			&tele.SendOptions{ParseMode: tele.ModeHTML})
	}

	return c.Send("Done reflecting. Use /facts to see what I wrote.")
}

// handleReflections shows recent reflections — Mira's journal entries
// from meaningful conversations. Stored in the dedicated reflections table.
func (b *Bot) handleReflections(c tele.Context) error {
	// Get all reflections (not just since a timestamp — show recent ones).
	reflections, err := b.store.ReflectionsSince(time.Time{}) // zero time = all
	if err != nil || len(reflections) == 0 {
		return c.Send("No reflections yet. Reflections happen after memory-dense conversations.")
	}

	// Show the most recent 5 (newest first).
	start := len(reflections) - 5
	if start < 0 {
		start = 0
	}
	recent := reflections[start:]

	var msg strings.Builder
	msg.WriteString("\U0001F4AD <b>Recent Reflections</b>\n\n")
	// Reverse to show newest first.
	for i := len(recent) - 1; i >= 0; i-- {
		r := recent[i]
		ts := r.Timestamp.Format("Jan 2, 3:04 PM")
		text := r.Content
		if len(text) > 250 {
			text = text[:250] + "..."
		}
		msg.WriteString(fmt.Sprintf("<b>%s</b>\n<i>%s</i>\n\n", ts, text))
	}

	msg.WriteString(fmt.Sprintf("<i>%d total reflections</i>", len(reflections)))
	return c.Send(msg.String(), &tele.SendOptions{ParseMode: tele.ModeHTML})
}

// handlePersona shows the current persona.md content.
func (b *Bot) handlePersona(c tele.Context) error {
	args := strings.TrimSpace(c.Message().Payload)

	if args == "history" {
		return b.handlePersonaHistory(c)
	}
	if args == "traits" {
		return b.handlePersonaTraits(c)
	}
	if args == "rewrite" || args == "write" {
		return b.handlePersonaRewrite(c)
	}

	data, err := os.ReadFile(b.cfg.Persona.PersonaFile)
	if err != nil || len(data) == 0 {
		return c.Send("No persona description yet. I'll develop one as we keep chatting!")
	}

	msg := fmt.Sprintf("\U0001FA9E <b>Who I Am Right Now</b>\n\n<i>%s</i>", string(data))
	return c.Send(msg, &tele.SendOptions{ParseMode: tele.ModeHTML})
}

// handlePersonaTraits shows current personality trait scores as a
// visual dashboard with emoji progress bars.
func (b *Bot) handlePersonaTraits(c tele.Context) error {
	traits, err := b.store.GetCurrentTraits()
	if err != nil || len(traits) == 0 {
		return c.Send("No trait scores yet. Traits are extracted after persona rewrites — keep chatting and they'll appear!")
	}

	var msg strings.Builder
	msg.WriteString("\U0001F3AD <b>Personality Traits</b>\n\n")

	for _, t := range traits {
		if t.TraitName == "humor_style" {
			msg.WriteString(fmt.Sprintf("<b>Humor style:</b> %s\n", t.Value))
			continue
		}

		// Parse float and build a 10-char progress bar.
		f := 0.0
		fmt.Sscanf(t.Value, "%f", &f)
		filled := int(f * 10)
		if filled > 10 {
			filled = 10
		}
		bar := strings.Repeat("█", filled) + strings.Repeat("░", 10-filled)

		// Title-case the trait name.
		displayName := strings.ToUpper(t.TraitName[:1]) + t.TraitName[1:]
		msg.WriteString(fmt.Sprintf("<code>%-11s %s</code> %s\n", displayName, bar, t.Value))
	}

	msg.WriteString(fmt.Sprintf("\n<i>Updated: persona v%d</i>", traits[0].PersonaVersionID))

	return c.Send(msg.String(), &tele.SendOptions{ParseMode: tele.ModeHTML})
}

// handlePersonaHistory shows past persona versions.
func (b *Bot) handlePersonaHistory(c tele.Context) error {
	versions, err := b.store.PersonaHistory(5)
	if err != nil || len(versions) == 0 {
		return c.Send("No persona history yet. My personality hasn't been rewritten yet!")
	}

	var msg strings.Builder
	msg.WriteString("\U0001FA9E <b>Persona History</b>\n\n")
	for _, v := range versions {
		msg.WriteString(fmt.Sprintf("<b>v%d</b> \u2014 %s\n<i>Trigger: %s</i>\n",
			v.ID, v.Timestamp.Format("Jan 2, 3:04 PM"), v.Trigger))
		content := v.Content
		if len(content) > 150 {
			content = content[:150] + "..."
		}
		msg.WriteString(fmt.Sprintf("<code>%s</code>\n\n", content))
	}

	return c.Send(msg.String(), &tele.SendOptions{ParseMode: tele.ModeHTML})
}

// handleDream manually triggers a full dream cycle — nightly reflection +
// gated persona rewrite — bypassing all cooldown gates. Equivalent to what
// the dreamer goroutine does at 04:00, but on demand. Useful after a
// particularly significant conversation or for testing the dreaming pipeline.
func (b *Bot) handleDream(c tele.Context) error {
	_ = c.Notify(tele.Typing)

	// Run nightly reflection immediately (no timing gate on manual trigger).
	if err := persona.NightlyReflect(b.llm, b.store, b.cfg, b.cfg.Identity.Her, b.cfg.Identity.User); err != nil {
		log.Error("dream reflection", "err", err)
		return c.Send(fmt.Sprintf("Reflection failed: %v", err))
	}

	// Run rewrite with bypass=true — skip both the 7-day and 3-reflection gates.
	minDays := b.cfg.Persona.MinRewriteDays
	if minDays == 0 {
		minDays = 7
	}
	minRefl := b.cfg.Persona.MinReflections
	if minRefl == 0 {
		minRefl = 3
	}
	rewritten, err := persona.GatedRewrite(b.llm, b.store, b.cfg.Persona.PersonaFile, b.cfg.Identity.Her, true, minDays, minRefl)
	if err != nil {
		log.Error("dream rewrite", "err", err)
		return c.Send(fmt.Sprintf("Rewrite failed: %v", err))
	}

	// Show the fresh reflection (saved in the last 30 seconds) and rewrite status.
	reflections, _ := b.store.ReflectionsSince(time.Now().Add(-30 * time.Second))
	var msg strings.Builder
	msg.WriteString("\U0001F4AD <b>Dream complete</b>\n\n")
	if len(reflections) > 0 {
		msg.WriteString(fmt.Sprintf("<i>%s</i>\n\n", reflections[len(reflections)-1].Content))
	} else {
		msg.WriteString("<i>Nothing notable to reflect on right now.</i>\n\n")
	}
	if rewritten {
		msg.WriteString("✨ Persona rewritten. Use /persona to see the update.")
	} else {
		msg.WriteString("<i>No persona changes — not enough has shifted yet.</i>")
	}
	return c.Send(msg.String(), &tele.SendOptions{ParseMode: tele.ModeHTML})
}

// handlePersonaRewrite manually triggers a persona rewrite + trait extraction.
// Bypasses the normal threshold checks — useful for testing or when you
// want to force an evolution after a meaningful conversation.
func (b *Bot) handlePersonaRewrite(c tele.Context) error {
	_ = c.Notify(tele.Typing)

	rewritten, err := persona.MaybeRewrite(b.llm, b.store, b.cfg.Persona.PersonaFile, 0, b.cfg.Identity.Her)
	if err != nil {
		log.Error("manual persona rewrite", "err", err)
		return c.Send(fmt.Sprintf("Rewrite failed: %v", err))
	}
	if !rewritten {
		return c.Send("Rewrite ran but nothing changed. This shouldn't happen — check the logs.")
	}

	// Read the freshly written persona.
	data, err := os.ReadFile(b.cfg.Persona.PersonaFile)
	if err != nil {
		return c.Send("Persona rewritten but I couldn't read it back. Check persona.md.")
	}

	// Show the new persona + traits if they were extracted.
	msg := fmt.Sprintf("\u2728 <b>Persona Rewritten</b>\n\n<i>%s</i>", string(data))

	traits, _ := b.store.GetCurrentTraits()
	if len(traits) > 0 {
		msg += "\n\n\U0001F3AD <b>Traits updated</b> — use /persona traits to see them."
	}

	return c.Send(msg, &tele.SendOptions{ParseMode: tele.ModeHTML})
}
