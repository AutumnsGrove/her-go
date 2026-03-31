package memory

import (
	"fmt"
	"regexp"
	"strings"

	"her/embed"
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

// conversationRedundancyThreshold controls how similar a fact must be to
// a recent message before it's considered redundant. This is cosine
// SIMILARITY (not distance) — 1.0 = identical, 0.0 = unrelated.
// 0.60 is intentionally lower than the fact-vs-fact dedup threshold
// (0.85) because we're comparing structured facts against freeform
// conversation text, which naturally score lower on cosine similarity
// even when they convey the same information.
const conversationRedundancyThreshold = 0.60

// FilterRedundantFacts removes facts whose content is already present in
// the recent conversation history. This prevents "context echo" — where
// a fact and a recent message both say the same thing, causing the chat
// model to fixate on that content and regurgitate it even when the
// current turn is about something different.
//
// How it works: each recent message is embedded, and each candidate
// fact's text is compared (via cosine similarity) against every message
// embedding. If a fact is too similar to any message, it's dropped.
//
// embedClient may be nil — if so, no filtering is performed.
// recentMessages is the same slice used to build the conversation
// history in the chat prompt.
func FilterRedundantFacts(facts []Fact, recentMessages []Message, embedClient *embed.Client) []Fact {
	if embedClient == nil || len(recentMessages) == 0 || len(facts) == 0 {
		return facts
	}

	// Embed each recent message. We use the scrubbed content (same as
	// what the chat model sees) so the comparison is apples-to-apples.
	var msgVecs [][]float32
	for _, msg := range recentMessages {
		content := msg.ContentScrubbed
		if content == "" {
			content = msg.ContentRaw
		}
		// Skip very short messages — greetings and "ok" don't carry
		// enough signal to meaningfully match against facts.
		if len(content) < 20 {
			continue
		}
		vec, err := embedClient.Embed(content)
		if err != nil {
			continue
		}
		msgVecs = append(msgVecs, vec)
	}

	if len(msgVecs) == 0 {
		return facts
	}

	var filtered []Fact
	for _, f := range facts {
		// Use the cached text embedding when available (populated by SaveFact
		// and UpdateFactEmbedding). Fall back to computing on-the-fly for
		// older facts that predate the embedding_text column.
		// We compare fact TEXT (not tags) against messages — we want semantic
		// overlap between what the fact *says* and what the conversation contains.
		factVec := f.EmbeddingText
		if len(factVec) == 0 {
			var err error
			factVec, err = embedClient.Embed(f.Fact)
			if err != nil {
				// Can't check — keep the fact to be safe.
				filtered = append(filtered, f)
				continue
			}
		}

		redundant := false
		var bestSim float64
		for _, msgVec := range msgVecs {
			sim := embed.CosineSimilarity(factVec, msgVec)
			if sim > bestSim {
				bestSim = sim
			}
			if sim >= conversationRedundancyThreshold {
				redundant = true
				break
			}
		}

		if redundant {
			factPreview := f.Fact
			if len(factPreview) > 60 {
				factPreview = factPreview[:60] + "..."
			}
			log.Infof("  [fact filtered: conversation redundancy] #%d sim=%.3f — %s", f.ID, bestSim, factPreview)
		} else {
			filtered = append(filtered, f)
		}
	}

	return filtered
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
				// Preserve the fact's source — "semantic" for direct KNN hits,
				// "linked" for facts pulled in via Zettelkasten 1-hop traversal.
				source := "semantic"
				if f.Source == "linked" {
					source = "linked"
				}
				injected = append(injected, InjectedFact{
					ID: f.ID, Fact: f.Fact, Category: f.Category,
					Subject: f.Subject, Importance: f.Importance,
					Distance: f.Distance, Source: source,
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

// legacyDatePrefix matches the [YYYY-MM-DD] prefix that was previously
// baked into "context" category fact text. Now that all facts get their
// timestamp rendered from the DB, we strip these to avoid double-stamping.
var legacyDatePrefix = regexp.MustCompile(`^\[\d{4}-\d{2}-\d{2}\]\s*`)

// formatFactSection builds a formatted memory section with a title,
// description, and facts grouped by category. Each fact is prefixed
// with its creation date from the DB so the chat model has temporal
// context — e.g., "[Mar 29] User prefers Lexend font".
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
			// Strip any legacy [YYYY-MM-DD] prefix from old context facts
			// so we don't double-stamp them.
			factText := legacyDatePrefix.ReplaceAllString(f.Fact, "")
			// Render the DB timestamp as a compact date prefix.
			// "Jan 02" format: abbreviated month + zero-padded day.
			stamp := f.Timestamp.Format("Jan 02")
			fmt.Fprintf(&b, "- [%s] %s\n", stamp, factText)
		}
		b.WriteString("\n")
	}

	return b.String()
}
