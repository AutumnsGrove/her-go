package persona

import (
	"fmt"
	"os"
	"strings"
	"time"

	"her/config"
	"her/llm"
	"her/memory"
	"her/tui"
)

// TomorrowPreloadParams bundles everything the preload agent needs.
type TomorrowPreloadParams struct {
	LLM      *llm.Client
	Store    memory.Store
	Cfg      *config.Config
	EventBus *tui.Bus
}

// RunTomorrowPreload generates a preload note for the next conversation.
// It gathers recent messages, calendar events, mood patterns, inbox tasks,
// and tonight's reflections, then asks the LLM to write 2-5 bullet points.
// The result is saved to the tomorrow_preload table.
func RunTomorrowPreload(p TomorrowPreloadParams) error {
	cfg := p.Cfg.Dream.TomorrowPreload
	if !cfg.Enabled {
		return nil
	}

	promptTemplate, err := os.ReadFile("persona/tomorrow_preload_prompt.md")
	if err != nil {
		return fmt.Errorf("reading preload prompt: %w", err)
	}

	// Gather context signals for the agent.
	var ctx strings.Builder

	// Recent messages — last N days of conversation.
	lookback := cfg.HistoryLookbackDays
	if lookback <= 0 {
		lookback = 7
	}
	msgs, err := p.Store.GlobalRecentMessages(100)
	if err == nil && len(msgs) > 0 {
		cutoff := time.Now().AddDate(0, 0, -lookback)
		ctx.WriteString("### Recent messages (last ")
		ctx.WriteString(fmt.Sprintf("%d days)\n\n", lookback))
		count := 0
		for _, m := range msgs {
			if m.Timestamp.Before(cutoff) {
				continue
			}
			content := m.ContentScrubbed
			if content == "" {
				content = m.ContentRaw
			}
			// Truncate long messages to keep context manageable.
			if len(content) > 200 {
				content = content[:200] + "..."
			}
			fmt.Fprintf(&ctx, "- [%s] %s: %s\n", m.Timestamp.Format("Jan 2"), m.Role, content)
			count++
			if count >= 50 {
				break
			}
		}
		ctx.WriteString("\n")
	}

	// Tomorrow's calendar events.
	tomorrow := time.Now().AddDate(0, 0, 1)
	tomorrowStart := time.Date(tomorrow.Year(), tomorrow.Month(), tomorrow.Day(), 0, 0, 0, 0, tomorrow.Location())
	tomorrowEnd := tomorrowStart.AddDate(0, 0, 1)
	events, err := p.Store.ListCalendarEvents(
		tomorrowStart.Format(time.RFC3339),
		tomorrowEnd.Format(time.RFC3339),
		"", false,
	)
	if err == nil && len(events) > 0 {
		ctx.WriteString("### Tomorrow's calendar\n\n")
		for _, e := range events {
			fmt.Fprintf(&ctx, "- %s (%s)\n", e.Title, e.Start.Format("3:04 PM"))
		}
		ctx.WriteString("\n")
	}

	// Open inbox tasks.
	pendingCount, _ := p.Store.PendingInboxCount("memory_agent")
	if pendingCount > 0 {
		fmt.Fprintf(&ctx, "### Open inbox tasks: %d pending\n\n", pendingCount)
	}

	// Recent mood patterns.
	recentMood, err := p.Store.RecentMoodEntries(memory.MoodKindMomentary, 7)
	if err == nil && len(recentMood) > 0 {
		ctx.WriteString("### Recent mood (last 7 entries)\n\n")
		for _, m := range recentMood {
			labels := "unlabeled"
			if len(m.Labels) > 0 {
				labels = strings.Join(m.Labels, ", ")
			}
			fmt.Fprintf(&ctx, "- %s: valence=%d (%s)\n", m.Timestamp.Format("Jan 2"), m.Valence, labels)
		}
		ctx.WriteString("\n")
	}

	// Tonight's reflections.
	reflections, err := p.Store.RecentReflections(3)
	if err == nil && len(reflections) > 0 {
		ctx.WriteString("### Tonight's reflections\n\n")
		for _, r := range reflections {
			content := r.Content
			if len(content) > 300 {
				content = content[:300] + "..."
			}
			fmt.Fprintf(&ctx, "- %s\n", content)
		}
		ctx.WriteString("\n")
	}

	// Current persona summary.
	personaContent, err := os.ReadFile(p.Cfg.Persona.PersonaFile)
	if err == nil && len(personaContent) > 0 {
		ctx.WriteString("### Current persona summary\n\n")
		summary := string(personaContent)
		if len(summary) > 500 {
			summary = summary[:500] + "..."
		}
		ctx.WriteString(summary)
		ctx.WriteString("\n\n")
	}

	// Build the prompt with context injected.
	prompt := strings.ReplaceAll(string(promptTemplate), "{{context}}", ctx.String())
	prompt = strings.ReplaceAll(prompt, "{{her}}", p.Cfg.Identity.Her)
	prompt = strings.ReplaceAll(prompt, "{{user}}", p.Cfg.Identity.User)

	// Call the LLM.
	resp, err := p.LLM.ChatCompletion([]llm.ChatMessage{
		{Role: "system", Content: prompt},
		{Role: "user", Content: "Write the preload note for tomorrow."},
	})
	if err != nil {
		return fmt.Errorf("preload LLM call: %w", err)
	}

	content := strings.TrimSpace(resp.Content)

	// If the agent says nothing notable, skip saving.
	if content == "NOTHING_NOTABLE" || content == "" {
		log.Info("preload: nothing notable for tomorrow")
		emitPersonaEvent(p.EventBus, "dream_preload", "nothing notable")
		return nil
	}

	// Save with configured expiry.
	expiryHours := cfg.ExpiresAfterHours
	if expiryHours <= 0 {
		expiryHours = 48
	}
	id, err := p.Store.SaveTomorrowPreload(content, time.Duration(expiryHours)*time.Hour)
	if err != nil {
		return fmt.Errorf("saving preload: %w", err)
	}

	log.Infof("preload: saved ID=%d (%d chars)", id, len(content))
	emitPersonaEvent(p.EventBus, "dream_preload", fmt.Sprintf("saved %d chars", len(content)))

	// Log cost.
	if p.Store != nil && resp.Model != "" {
		p.Store.SaveMetric(resp.Model, resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, 0, 0, resp.UsedFallback, memory.RoleDream)
	}

	return nil
}
