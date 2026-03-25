package memory

import (
	"fmt"
	"strings"
)

// InjectedFact records which fact was included in the chat model's prompt
// and why. This is the observability data that lets you debug "why did she
// mention pollen when I asked about code?"
type InjectedFact struct {
	ID         int64
	Fact       string
	Category   string
	Subject    string
	Importance int
	Distance   float64 // cosine distance from query (0 = identical, only set for semantic)
	Source     string  // "semantic" or "importance" — how this fact got selected
}

// BuildMemoryContext assembles a memory context string to inject into
// the system prompt. It blends two retrieval strategies:
//
//  1. Semantic: top-K facts closest to the user's current message (via sqlite-vec KNN)
//  2. Importance: high-importance facts that should always be present (name, identity, etc.)
//
// The blend ensures the bot always knows WHO it's talking to (importance) while also
// recalling context RELEVANT to the current conversation (semantic).
//
// Returns the formatted context string AND a list of which facts were injected
// (with their scores and selection source) for observability.
//
// relevantFacts can be nil if embeddings aren't available — falls back to
// importance-only retrieval (the pre-v0.4 behavior).
// maxSemanticDist is the cosine distance cutoff — facts farther than this
// from the query are filtered out even if they're the "nearest" neighbors.
// Set to 0 to disable filtering (include all KNN results).
func BuildMemoryContext(store *Store, maxFacts int, relevantFacts []Fact, userName string, maxSemanticDist float64) (string, []InjectedFact, error) {
	var parts []string
	var allInjected []InjectedFact

	// --- User facts ---
	// User facts are ONLY injected when semantically relevant to the current
	// message. If nothing passes the distance filter, we inject nothing —
	// Mira can use her recall tool if she needs more context.
	// Importance-based backfill was flooding irrelevant facts into every turn.
	userFacts, userInjected, err := blendFacts(store, "user", maxFacts, relevantFacts, maxSemanticDist, false)
	if err != nil {
		return "", nil, fmt.Errorf("retrieving user facts: %w", err)
	}
	allInjected = append(allInjected, userInjected...)
	if len(userFacts) > 0 {
		parts = append(parts, formatFactSection(
			fmt.Sprintf("Things I Know About %s", userName),
			fmt.Sprintf("These are facts about %s, gathered from our past conversations. These are THEIR experiences, THEIR life, THEIR details — not mine. Use them to be a better friend, not to adopt their identity.", userName),
			userFacts,
		))
	}

	// --- Self facts ---
	// Self facts always backfill by importance — they steer Mira's personality
	// and voice, so they should be present even when nothing is semantically close.
	selfFacts, selfInjected, err := blendFacts(store, "self", maxFacts, relevantFacts, maxSemanticDist, true)
	if err != nil {
		return "", nil, fmt.Errorf("retrieving self facts: %w", err)
	}
	allInjected = append(allInjected, selfInjected...)
	if len(selfFacts) > 0 {
		parts = append(parts, formatFactSection(
			"Things I Know About Myself",
			"These are my own observations, patterns, and identity notes. Things I've learned about how I communicate, what works, and who I am.",
			selfFacts,
		))
	}

	return strings.Join(parts, "\n\n"), allInjected, nil
}

// blendFacts merges semantic search results with importance-based results,
// deduplicating by fact ID. Returns both the facts and observability data
// about how each was selected.
//
// If backfillImportance is true, remaining slots are filled with
// high-importance facts even when nothing is semantically close. This is
// useful for self-facts (personality steering) but not for user-facts,
// where irrelevant backfill just overwhelms the chat model.
//
// If relevantFacts is nil (no embeddings), falls back to importance-only
// when backfillImportance is true, or returns nothing when false.
func blendFacts(store *Store, subject string, maxFacts int, relevantFacts []Fact, maxDist float64, backfillImportance bool) ([]Fact, []InjectedFact, error) {
	seen := make(map[int64]bool)
	var result []Fact
	var injected []InjectedFact

	// First pass: semantic results for this subject (most relevant first).
	// These are the facts KNN says are closest to what the user just said.
	// Filter by maxDist — with a small fact set, KNN returns "nearest"
	// neighbors that are still completely irrelevant (dist 0.5+). Without
	// this filter, therapy cancellations show up when you ask about code.
	if relevantFacts != nil {
		for _, f := range relevantFacts {
			if f.Subject != subject {
				continue
			}
			// Skip facts that are too far from the query
			if maxDist > 0 && f.Distance > maxDist {
				factPreview := f.Fact
				if len(factPreview) > 50 {
					factPreview = factPreview[:50] + "..."
				}
				log.Infof("  [fact filtered] #%d dist=%.3f > max=%.3f — %s", f.ID, f.Distance, maxDist, factPreview)
				continue
			}
			if !seen[f.ID] {
				seen[f.ID] = true
				result = append(result, f)
				injected = append(injected, InjectedFact{
					ID: f.ID, Fact: f.Fact, Category: f.Category,
					Subject: f.Subject, Importance: f.Importance,
					Distance: f.Distance, Source: "semantic",
				})
			}
		}
	}

	// Second pass: importance-based (always-present context).
	// Only runs when backfillImportance is true (self-facts).
	// For user-facts this is skipped — Mira can use her recall tool
	// if she needs more context beyond what semantic search found.
	if backfillImportance {
		importantFacts, err := store.RecentFacts(subject, maxFacts)
		if err != nil {
			return nil, nil, err
		}
		for _, f := range importantFacts {
			if len(result) >= maxFacts {
				break
			}
			if !seen[f.ID] {
				seen[f.ID] = true
				result = append(result, f)
				injected = append(injected, InjectedFact{
					ID: f.ID, Fact: f.Fact, Category: f.Category,
					Subject: f.Subject, Importance: f.Importance,
					Distance: -1, // not from semantic search
					Source:   "importance",
				})
			}
		}
	}

	return result, injected, nil
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
