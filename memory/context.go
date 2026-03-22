package memory

import (
	"fmt"
	"strings"
)

// BuildMemoryContext assembles a memory context string to inject into
// the system prompt. This includes two sections:
//   - "Things I Know About the User" — facts about the person (subject="user")
//   - "Things I Know About Myself" — Mira's self-knowledge (subject="self")
func BuildMemoryContext(store *Store, maxFacts int) (string, error) {
	var parts []string

	// User facts — things Mira knows about the person she's talking to.
	userFacts, err := store.RecentFacts("user", maxFacts)
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

	// Self facts — Mira's own self-knowledge and observations.
	selfFacts, err := store.RecentFacts("self", maxFacts)
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
