package tools

import (
	"fmt"
	"time"

	"her/config"
	"her/memory"
)

// CanForget checks whether a memory is eligible for removal by the dream
// agent. Returns (true, "") if removal is allowed, or (false, reason) if
// the memory is protected. These are hard rules enforced in code — the
// dreamer prompt has matching soft rules, but even if the LLM ignores
// the prompt, the code guard catches it.
//
// Only applies when the caller is the dream agent (ctx.AgentName == "dream").
// The memory agent responding to user requests ("forget that") bypasses
// these checks — users can always delete their own data.
func CanForget(m *memory.Memory, store memory.Store, cfg config.ForgettingConfig) (bool, string) {
	// Protected cards: never forget anything inside them.
	if reason := isInProtectedCard(m.ID, store); reason != "" {
		return false, reason
	}

	// Importance floor: anything above the threshold stays unless
	// explicitly superseded (which goes through SupersedeMemory, not
	// remove_memory).
	maxImportance := cfg.RequireLowImportance
	if maxImportance <= 0 {
		maxImportance = 3
	}
	if m.Importance > maxImportance {
		return false, fmt.Sprintf("importance %d > %d", m.Importance, maxImportance)
	}

	// Head of supersession chain: don't orphan predecessors.
	if hasSupersessionPredecessors(m.ID, store) {
		return false, "head of supersession chain"
	}

	// Age floor: anything saved recently stays.
	minAge := cfg.MinAgeDays
	if minAge <= 0 {
		minAge = 60
	}
	if time.Since(m.Timestamp) < time.Duration(minAge)*24*time.Hour {
		return false, fmt.Sprintf("saved %d days ago (min %d)", int(time.Since(m.Timestamp).Hours()/24), minAge)
	}

	// Usage recency: anything recalled recently stays.
	minUnused := cfg.MinUnusedDays
	if minUnused <= 0 {
		minUnused = 60
	}
	if !m.LastRecalledAt.IsZero() && time.Since(m.LastRecalledAt) < time.Duration(minUnused)*24*time.Hour {
		return false, fmt.Sprintf("recalled %d days ago (min unused %d)", int(time.Since(m.LastRecalledAt).Hours()/24), minUnused)
	}

	return true, ""
}

// isInProtectedCard checks whether a memory belongs to a protected card.
// Uses a direct SQL join via MemoryCardForMemory — one query, no iteration.
func isInProtectedCard(memoryID int64, store memory.Store) string {
	card, err := store.MemoryCardForMemory(memoryID)
	if err != nil || card == nil {
		return "" // no card or error — not protected
	}
	if card.Protected {
		return fmt.Sprintf("card %q is protected", card.TopicSlug)
	}
	return ""
}

// hasSupersessionPredecessors checks if any other memory points to this
// one via superseded_by — meaning this memory is the "current" version
// in a chain. Removing it would orphan the history.
func hasSupersessionPredecessors(memoryID int64, store memory.Store) bool {
	history, err := store.MemoryHistory(memoryID)
	if err != nil {
		return false
	}
	return len(history) > 1
}
