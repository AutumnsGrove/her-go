package memory

import (
	"fmt"
	"strings"
)

// BuildMemoryContext assembles a memory context string to inject into
// the system prompt. It blends two retrieval strategies:
//
//  1. Semantic: top-K facts closest to the user's current message (via sqlite-vec KNN)
//  2. Importance: high-importance facts that should always be present (name, identity, etc.)
//
// The blend ensures Mira always knows WHO she's talking to (importance) while also
// recalling context RELEVANT to the current conversation (semantic).
//
// relevantFacts can be nil if embeddings aren't available — falls back to
// importance-only retrieval (the pre-v0.4 behavior).
func BuildMemoryContext(store *Store, maxFacts int, relevantFacts []Fact) (string, error) {
	var parts []string

	// --- User facts ---
	userFacts, err := blendFacts(store, "user", maxFacts, relevantFacts)
	if err != nil {
		return "", fmt.Errorf("retrieving user facts: %w", err)
	}
	if len(userFacts) > 0 {
		parts = append(parts, formatFactSection(
			"Things I Know About the User",
			"These are facts about the person I'm talking to, gathered from our past conversations. These are THEIR experiences, THEIR life, THEIR details — not mine. Use them to be a better friend, not to adopt their identity.",
			userFacts,
		))
	}

	// --- Self facts ---
	selfFacts, err := blendFacts(store, "self", maxFacts, relevantFacts)
	if err != nil {
		return "", fmt.Errorf("retrieving self facts: %w", err)
	}
	if len(selfFacts) > 0 {
		parts = append(parts, formatFactSection(
			"Things I Know About Myself",
			"These are my own observations, patterns, and identity notes. Things I've learned about how I communicate, what works, and who I am.",
			selfFacts,
		))
	}

	return strings.Join(parts, "\n\n"), nil
}

// blendFacts merges semantic search results with importance-based results,
// deduplicating by fact ID. The result set contains:
//   - Semantically relevant facts for this subject (from the KNN results)
//   - Top-importance facts for this subject (always-present context)
//
// If relevantFacts is nil (no embeddings), falls back to importance-only.
func blendFacts(store *Store, subject string, maxFacts int, relevantFacts []Fact) ([]Fact, error) {
	seen := make(map[int64]bool)
	var result []Fact

	// First pass: semantic results for this subject (most relevant first).
	// These are the facts KNN says are closest to what the user just said.
	if relevantFacts != nil {
		for _, f := range relevantFacts {
			if f.Subject != subject {
				continue
			}
			if !seen[f.ID] {
				seen[f.ID] = true
				result = append(result, f)
			}
		}
	}

	// Second pass: importance-based (always-present context).
	// Fill remaining slots with the highest-importance facts.
	importantFacts, err := store.RecentFacts(subject, maxFacts)
	if err != nil {
		return nil, err
	}
	for _, f := range importantFacts {
		if len(result) >= maxFacts {
			break
		}
		if !seen[f.ID] {
			seen[f.ID] = true
			result = append(result, f)
		}
	}

	return result, nil
}

// formatFactSection builds a formatted memory section with a title,
// description, and facts grouped by category.
func formatFactSection(title, description string, facts []Fact) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n%s\n\n", title, description)

	// Group facts by category.
	groups := make(map[string][]Fact)
	var order []string
	for _, f := range facts {
		cat := f.Category
		if cat == "" {
			cat = "other"
		}
		if _, exists := groups[cat]; !exists {
			order = append(order, cat)
		}
		groups[cat] = append(groups[cat], f)
	}

	for _, cat := range order {
		displayCat := strings.ToUpper(cat[:1]) + cat[1:]
		fmt.Fprintf(&b, "**%s:**\n", displayCat)
		for _, f := range groups[cat] {
			fmt.Fprintf(&b, "- %s\n", f.Fact)
		}
		b.WriteString("\n")
	}

	return b.String()
}
