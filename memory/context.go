package memory

import (
	"fmt"
	"regexp"
	"strings"

	"her/embed"
)

// InjectedMemory records which memory was included in the chat model's prompt
// and why. This is the observability data that lets you debug "why did she
// mention pollen when I asked about code?"
type InjectedMemory struct {
	ID       int64
	Content  string
	Category string
	Subject  string
	Distance float64 // cosine distance from query (0 = identical, only set for semantic)
	Source   string  // "semantic" or "linked" — how this memory got selected
}

// FilterRedundantMemories removes memories whose content is already present in
// the recent conversation history. This prevents "context echo" — where
// a memory and a recent message both say the same thing, causing the chat
// model to fixate on that content and regurgitate it even when the
// current turn is about something different.
//
// How it works: each recent message is embedded, and each candidate
// memory's text is compared (via cosine similarity) against every message
// embedding. If a memory is too similar to any message, it's dropped.
//
// embedClient may be nil — if so, no filtering is performed.
// recentMessages is the same slice used to build the conversation
// history in the chat prompt.
func FilterRedundantMemories(memories []Memory, recentMessages []Message, embedClient *embed.Client) []Memory {
	if embedClient == nil || len(recentMessages) == 0 || len(memories) == 0 {
		return memories
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
		// enough signal to meaningfully match against memories.
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
		return memories
	}

	// Build candidate map from message vectors once, reuse for all memories.
	// We use message index as ID since we only care about similarity, not which message.
	msgCandidates := make(map[int64][]float32, len(msgVecs))
	for i, vec := range msgVecs {
		msgCandidates[int64(i)] = vec
	}

	var filtered []Memory
	for _, m := range memories {
		// Use the cached text embedding when available (populated by SaveMemory
		// and UpdateMemoryEmbedding). Fall back to computing on-the-fly for
		// older memories that predate the embedding_text column.
		// We compare memory TEXT (not tags) against messages — we want semantic
		// overlap between what the memory *says* and what the conversation contains.
		memVec := m.EmbeddingText
		if len(memVec) == 0 {
			var err error
			memVec, err = embedClient.Embed(m.Content)
			if err != nil {
				// Can't check — keep the memory to be safe.
				filtered = append(filtered, m)
				continue
			}
		}

		// Use FindBestMatch with earlyExit=true for performance.
		// Chat latency matters, so we return as soon as we find ANY message that
		// exceeds the threshold. We don't need to know which message or the exact
		// best similarity — just "is this memory redundant?"
		_, bestSim, redundant := embed.FindBestMatch(memVec, msgCandidates, embed.ConversationRedundancyThreshold, true)

		if redundant {
			memPreview := m.Content
			if len(memPreview) > 60 {
				memPreview = memPreview[:60] + "..."
			}
			log.Infof("  [memory filtered: conversation redundancy] #%d sim=%.3f — %s", m.ID, bestSim, memPreview)
		} else {
			filtered = append(filtered, m)
		}
	}

	return filtered
}

// BuildMemoryContext assembles a memory context string to inject into
// the system prompt using semantic retrieval:
//
//   - Semantic: top-K memories closest to the user's current message (via sqlite-vec KNN)
//
// Returns the formatted context string AND a list of which memories were injected
// (with their scores and selection source) for observability.
//
// relevantMemories can be nil if embeddings aren't available — returns empty context.
// maxSemanticDist is the cosine distance cutoff — memories farther than this
// from the query are filtered out even if they're the "nearest" neighbors.
// Set to 0 to disable filtering (include all KNN results).
func BuildMemoryContext(store *Store, maxMemories int, relevantMemories []Memory, userName string, maxSemanticDist float64) (string, []InjectedMemory, error) {
	var parts []string
	var allInjected []InjectedMemory

	// --- User memories ---
	// User memories are ONLY injected when semantically relevant to the current
	// message. If nothing passes the distance filter, we inject nothing —
	// Mira can use her recall tool if she needs more context.
	// Importance-based backfill was flooding irrelevant memories into every turn.
	userMemories, userInjected, err := blendMemories(store, "user", maxMemories, relevantMemories, maxSemanticDist)
	if err != nil {
		return "", nil, fmt.Errorf("retrieving user memories: %w", err)
	}
	allInjected = append(allInjected, userInjected...)
	if len(userMemories) > 0 {
		parts = append(parts, formatMemorySection(
			fmt.Sprintf("Things I Know About %s", userName),
			fmt.Sprintf("These are facts about %s, gathered from our past conversations. These are THEIR experiences, THEIR life, THEIR details — not mine. Use them to be a better friend, not to adopt their identity.", userName),
			userMemories,
		))
	}

	// --- Self memories ---
	// Self memories use the same semantic-only retrieval as user memories.
	selfMemories, selfInjected, err := blendMemories(store, "self", maxMemories, relevantMemories, maxSemanticDist)
	if err != nil {
		return "", nil, fmt.Errorf("retrieving self memories: %w", err)
	}
	allInjected = append(allInjected, selfInjected...)
	if len(selfMemories) > 0 {
		parts = append(parts, formatMemorySection(
			"Things I Know About Myself",
			"These are my own observations, patterns, and identity notes. Things I've learned about how I communicate, what works, and who I am.",
			selfMemories,
		))
	}

	return strings.Join(parts, "\n\n"), allInjected, nil
}

// blendMemories filters semantic search results for a given subject,
// deduplicating by memory ID. Returns both the memories and observability data
// about how each was selected.
//
// If relevantMemories is nil (no embeddings), returns nothing.
func blendMemories(store *Store, subject string, maxMemories int, relevantMemories []Memory, maxDist float64) ([]Memory, []InjectedMemory, error) {
	seen := make(map[int64]bool)
	var result []Memory
	var injected []InjectedMemory

	// First pass: semantic results for this subject (most relevant first).
	// These are the memories KNN says are closest to what the user just said.
	// Filter by maxDist — with a small memory set, KNN returns "nearest"
	// neighbors that are still completely irrelevant (dist 0.5+). Without
	// this filter, therapy cancellations show up when you ask about code.
	if relevantMemories != nil {
		for _, m := range relevantMemories {
			if m.Subject != subject {
				continue
			}
			// Skip memories that are too far from the query
			if maxDist > 0 && m.Distance > maxDist {
				memPreview := m.Content
				if len(memPreview) > 50 {
					memPreview = memPreview[:50] + "..."
				}
				log.Infof("  [memory filtered] #%d dist=%.3f > max=%.3f — %s", m.ID, m.Distance, maxDist, memPreview)
				continue
			}
			if !seen[m.ID] {
				seen[m.ID] = true
				result = append(result, m)
				// Preserve the memory's source — "semantic" for direct KNN hits,
				// "linked" for memories pulled in via Zettelkasten 1-hop traversal.
				source := "semantic"
				if m.Source == "linked" {
					source = "linked"
				}
				injected = append(injected, InjectedMemory{
					ID: m.ID, Content: m.Content, Category: m.Category,
					Subject: m.Subject,
					Distance: m.Distance, Source: source,
				})
			}
		}
	}

	return result, injected, nil
}

// legacyDatePrefix matches the [YYYY-MM-DD] prefix that was previously
// baked into "context" category memory text. Now that all memories get their
// timestamp rendered from the DB, we strip these to avoid double-stamping.
var legacyDatePrefix = regexp.MustCompile(`^\[\d{4}-\d{2}-\d{2}\]\s*`)

// formatMemorySection builds a formatted memory section with a title,
// description, and memories grouped by category. Each memory is prefixed
// with its creation date from the DB so the chat model has temporal
// context — e.g., "[Mar 29] User prefers Lexend font".
func formatMemorySection(title, description string, memories []Memory) string {
	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n%s\n\n", title, description)

	// Group memories by category.
	groups := make(map[string][]Memory)
	var order []string
	for _, m := range memories {
		cat := m.Category
		if cat == "" {
			cat = "other"
		}
		if _, exists := groups[cat]; !exists {
			order = append(order, cat)
		}
		groups[cat] = append(groups[cat], m)
	}

	for _, cat := range order {
		displayCat := strings.ToUpper(cat[:1]) + cat[1:]
		fmt.Fprintf(&b, "**%s:**\n", displayCat)
		for _, m := range groups[cat] {
			// Strip any legacy [YYYY-MM-DD] prefix from old context memories
			// so we don't double-stamp them.
			memText := legacyDatePrefix.ReplaceAllString(m.Content, "")
			// Render the DB timestamp as a compact date prefix.
			// "Jan 02" format: abbreviated month + zero-padded day.
			stamp := m.Timestamp.Format("Jan 02")
			// Include context in parentheses when present — gives the chat
			// model the "why" alongside the "what".
			if m.Context != "" {
				fmt.Fprintf(&b, "- [%s] %s (%s)\n", stamp, memText, m.Context)
			} else {
				fmt.Fprintf(&b, "- [%s] %s\n", stamp, memText)
			}
		}
		b.WriteString("\n")
	}

	return b.String()
}
