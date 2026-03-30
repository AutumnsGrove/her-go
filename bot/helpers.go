// Package bot — shared helpers and utility functions.
package bot

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"her/memory"
	"her/tools"

	tele "gopkg.in/telebot.v4"
)

// truncate shortens a string for log output, adding "..." if it was cut.
func truncate(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", " ") // flatten newlines for single-line logs
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// stripHTML removes HTML tags for a plain-text fallback when Telegram's
// HTML parser rejects our formatting. Crude but effective for traces.
func stripHTML(s string) string {
	s = strings.ReplaceAll(s, "<b>", "")
	s = strings.ReplaceAll(s, "</b>", "")
	s = strings.ReplaceAll(s, "<i>", "")
	s = strings.ReplaceAll(s, "</i>", "")
	s = strings.ReplaceAll(s, "&amp;", "&")
	s = strings.ReplaceAll(s, "&lt;", "<")
	s = strings.ReplaceAll(s, "&gt;", ">")
	return s
}

// formatTokens formats a token count with K/M suffixes for readability.
func formatTokens(n int) string {
	if n >= 1_000_000 {
		return fmt.Sprintf("%.1fM", float64(n)/1_000_000)
	}
	if n >= 1_000 {
		return fmt.Sprintf("%.1fK", float64(n)/1_000)
	}
	return fmt.Sprintf("%d", n)
}

// getConversationID returns the active conversation ID for a chat.
// On first call after a restart, it checks the database for the most
// recent conversation ID for this chat, so the bot resumes where it
// left off instead of starting a new conversation and losing context.
func (b *Bot) getConversationID(chatID int64) string {
	key := fmt.Sprintf("%d", chatID)

	// Check in-memory cache first.
	if val, ok := b.conversationIDs.Load(key); ok {
		return val.(string)
	}

	// Not in memory (first message after restart). Check the DB
	// for the most recent conversation with this chat.
	prefix := fmt.Sprintf("tg_%d", chatID)
	if existing := b.store.LatestConversationID(prefix); existing != "" {
		b.conversationIDs.Store(key, existing)
		log.Info("resumed conversation", "id", existing)
		return existing
	}

	// No existing conversation. Create a new one.
	newID := fmt.Sprintf("tg_%d_%d", chatID, time.Now().Unix())
	b.conversationIDs.Store(key, newID)
	return newID
}

// makeTraceCallback creates a closure that sends/edits the agent trace
// message in Telegram. First call sends a new message; subsequent calls
// edit it with the accumulated trace text. The message uses HTML parse
// mode for formatting (bold tool names, italic thinking, etc.).
//
// This is the same closure pattern as statusCallback and sendCallback —
// the returned function "closes over" the traceMsg variable so it
// always knows which message to edit.
func (b *Bot) makeTraceCallback(c tele.Context) tools.TraceCallback {
	// Pre-send a placeholder so the trace message is ABOVE the reply
	// in chat order. It gets replaced on the first real trace update.
	// Uses a short-timeout client so a Telegram blip doesn't stall the
	// entire agent pipeline (the 60s default caused an 85s stall).
	traceMsg, err := c.Bot().Send(c.Recipient(), "🧠")
	if err != nil {
		log.Warn("trace: failed to send placeholder", "err", err)
		traceMsg = nil
	}

	// All trace operations run in a goroutine so they NEVER block the
	// agent loop. Traces are observability — not critical path. A mutex
	// preserves ordering so rapid tool calls don't race.
	var mu sync.Mutex
	return func(text string) error {
		go func() {
			mu.Lock()
			defer mu.Unlock()

			if traceMsg == nil {
				msg, err := c.Bot().Send(c.Recipient(), text, &tele.SendOptions{ParseMode: tele.ModeHTML})
				if err != nil {
					log.Warn("trace: send failed", "err", err)
					msg, err = c.Bot().Send(c.Recipient(), stripHTML(text))
					if err != nil {
						log.Warn("trace: plain send also failed", "err", err)
						return
					}
				}
				traceMsg = msg
			} else {
				_, err := c.Bot().Edit(traceMsg, text, &tele.SendOptions{ParseMode: tele.ModeHTML})
				if err != nil {
					if strings.Contains(err.Error(), "not modified") {
						return
					}
					log.Warn("trace: edit failed, retrying plain", "err", err)
					_, err = c.Bot().Edit(traceMsg, stripHTML(text))
					if err != nil && !strings.Contains(err.Error(), "not modified") {
						log.Warn("trace: plain edit also failed", "err", err)
					}
				}
			}
		}()
		return nil
	}
}

// handleTraces toggles agent thinking traces on/off.
// When enabled, Mira sends a separate message before each reply showing
// the agent's tool calls, thinking, and decision-making process.
func (b *Bot) handleTraces(c tele.Context) error {
	newState := !b.cfg.Agent.Trace
	if err := b.cfg.SetTrace(b.configPath, newState); err != nil {
		log.Error("/traces: failed to update config", "err", err)
		return c.Send(fmt.Sprintf("Failed to update config: %v", err))
	}
	if newState {
		return c.Send("🧠 Agent traces <b>enabled</b> — you'll see thinking traces before each reply.", &tele.SendOptions{ParseMode: tele.ModeHTML})
	}
	return c.Send("🧠 Agent traces <b>disabled</b>.", &tele.SendOptions{ParseMode: tele.ModeHTML})
}

// buildSystemPrompt assembles the full system prompt by reading prompt.md
// fresh from disk (hot-reloadable), then layering in persona.md and
// memory context (extracted facts).
//
// This is still used by /reflect which calls the conversational model
// directly. The main message pipeline now uses the agent's buildChatSystemPrompt.
func (b *Bot) buildSystemPrompt() string {
	var parts []string

	// Layer 1: prompt.md — base identity (hot-reloaded from disk).
	if promptBytes, err := os.ReadFile(b.cfg.Persona.PromptFile); err == nil {
		parts = append(parts, string(promptBytes))
	} else {
		parts = append(parts, b.systemPrompt)
	}

	// Layer 2: persona.md — evolving self-image (if it exists).
	if personaBytes, err := os.ReadFile(b.cfg.Persona.PersonaFile); err == nil {
		parts = append(parts, string(personaBytes))
	}

	// Layer 4: Memory context — extracted facts about the user.
	if memCtx, _, err := memory.BuildMemoryContext(b.store, b.cfg.Memory.MaxFactsInContext, nil, b.cfg.Identity.User, b.cfg.Embed.MaxSemanticDistance); err == nil && memCtx != "" {
		parts = append(parts, memCtx)
	}

	return strings.Join(parts, "\n\n---\n\n")
}
