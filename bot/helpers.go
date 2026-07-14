// Package bot — shared helpers and utility functions.
package bot

import (
	"fmt"
	"os"
	"strings"
	"time"

	"her/memory"
	"her/scrub"
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

	// Fast path: already cached (no lock needed).
	if val, ok := b.conversationIDs.Load(key); ok {
		return val.(string)
	}

	// Slow path: DB lookup + possible creation. The mutex serialises
	// this so two concurrent messages for the same chat can't both
	// create new conversation IDs.
	b.conversationIDsMu.Lock()
	defer b.conversationIDsMu.Unlock()

	// Re-check after acquiring the lock — another goroutine may have
	// populated the cache while we were waiting.
	if val, ok := b.conversationIDs.Load(key); ok {
		return val.(string)
	}

	prefix := fmt.Sprintf("tg_%d", chatID)
	if existing := b.store.LatestConversationID(prefix); existing != "" {
		b.conversationIDs.Store(key, existing)
		log.Info("resumed conversation", "id", existing)
		return existing
	}

	newID := fmt.Sprintf("tg_%d_%d", chatID, time.Now().Unix())
	b.conversationIDs.Store(key, newID)
	return newID
}

// ConversationID is the exported form of getConversationID, for callers
// outside the bot package (the scheduler, via scheduler.Deps.GetConversationID)
// that need to route a proactively-sent message into the same conversation
// the driver agent is actually reading from.
func (b *Bot) ConversationID(chatID int64) string {
	return b.getConversationID(chatID)
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

// scrubText applies PII scrubbing to user text based on the bot's config.
// Returns a ScrubResult with the scrubbed text and vault. If scrubbing is
// disabled, the text passes through unchanged with an empty vault.
//
// This consolidates the repeated if/else pattern from handleMessage,
// handlePhoto, and handleVoice.
func (b *Bot) scrubText(text string) *scrub.ScrubResult {
	if b.cfg.Scrub.Enabled {
		result := scrub.Scrub(text)
		if vaultCount := len(result.Vault.Entries()); vaultCount > 0 {
			log.Info("PII scrubbed", "tokens", vaultCount)
		}
		return result
	}
	return &scrub.ScrubResult{
		Text:  text,
		Vault: scrub.NewVault(),
	}
}

// savePIIVaultEntries persists all vault entries for a message to the DB.
// Called after scrubbing to store the token↔original mapping for later
// deanonymization.
func (b *Bot) savePIIVaultEntries(msgID int64, vault *scrub.Vault) {
	for _, entry := range vault.Entries() {
		if err := b.store.SavePIIVaultEntry(msgID, entry.Token, entry.Original, entry.EntityType); err != nil {
			log.Error("saving PII vault entry", "err", err)
		}
	}
}
