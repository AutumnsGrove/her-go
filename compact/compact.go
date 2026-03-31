// Package compact handles conversation history compaction.
//
// As conversations grow, the token count of the history window increases.
// Left unchecked, this fills the context window and degrades model quality
// (research shows ~23% quality drop above 85% context utilization).
//
// The approach: sliding window + running summary hybrid.
//   - Recent messages stay in full fidelity (you need exact wording for context)
//   - Older messages get summarized into a running summary by the LLM
//   - The summary is stored in SQLite and injected before recent messages
//   - Facts already capture the important long-term stuff, so the summary
//     only needs to preserve conversational flow and context
//
// This is the same pattern used by LangChain's ConversationSummaryBufferMemory,
// MemGPT/Letta, and Goose. It works because recent context matters more than
// old context, and facts handle the truly important stuff.
package compact

import (
	"fmt"
	"strings"

	"her/llm"
	"her/logger"
	"her/memory"
)

// log is the package-level logger for the compact package.
var log = logger.WithPrefix("compact")

// estimateTokens gives a rough token count for a string.
// The rule of thumb is ~4 characters per token for English text.
// This is intentionally approximate. We don't need exact counts,
// just enough to know when we're approaching the budget.
//
// In Python you'd use tiktoken for exact counts. In Go, there are
// tiktoken ports, but for our use case the 4-char estimate is fine
// and has zero dependencies.
func estimateTokens(s string) int {
	return len(s) / 4
}

// EstimateHistoryTokens calculates the approximate token count of
// a conversation history, including any existing summary.
//
// For assistant messages with a real TokenCount (from CompletionTokens),
// we use that directly instead of estimating. User messages always use
// estimation because their TokenCount stores total prompt size, not
// per-message size. Messages without token data (TokenCount == 0) fall
// back to the len/4 heuristic.
func EstimateHistoryTokens(summary string, messages []memory.Message) int {
	total := estimateTokens(summary)
	for _, msg := range messages {
		// Assistant messages with real token counts: use directly.
		// Their TokenCount is CompletionTokens — the actual response size.
		if msg.Role == "assistant" && msg.TokenCount > 0 {
			total += msg.TokenCount
			total += 10 // overhead for role markers, formatting
			continue
		}
		content := msg.ContentScrubbed
		if content == "" {
			content = msg.ContentRaw
		}
		total += estimateTokens(content)
		total += 10 // overhead for role markers, formatting
	}
	return total
}

// summaryPrompt is sent to the LLM to summarize older messages.
// It's designed to preserve conversational flow and emotional context
// while being much shorter than the raw messages.
// summaryPromptTmpl uses %s placeholders: userName, botName.
const summaryPromptTmpl = `You are summarizing an earlier part of a conversation between %s and %s (an AI companion). Your goal is to capture what matters for continuing the conversation naturally.

Preserve:
- What topics were discussed and any conclusions reached
- Emotional tone and how the conversation felt
- Any commitments, plans, or things either person said they'd do
- Context needed to understand references in later messages
- The general arc of the conversation

Don't preserve:
- Exact wording — ALWAYS paraphrase in your own words, never copy phrases, metaphors, or specific advice verbatim
- Greetings and small talk unless they established something important
- Repetitive back-and-forth that can be summarized in one line
- Information already captured in the facts/memories system

Write the summary as a brief narrative, like you're catching up a friend who missed the first part of the conversation. Keep it concise. 2-4 sentences for a short exchange, 4-8 for a longer one.

If there's an existing summary of even earlier conversation, incorporate it naturally into your new summary. Don't just append, weave it together.`

// CompactResult holds the output of MaybeCompact so callers can tell
// whether compaction actually ran (vs. just returning existing state).
type CompactResult struct {
	Summary      string           // running summary (may be empty if no history)
	KeptMessages []memory.Message // messages that should stay in full fidelity
	DidCompact   bool             // true if new summarization was performed this call
	Summarized   int              // number of messages that were summarized (0 if no compaction)
	TokensBefore int              // estimated tokens before compaction
	TokensAfter  int              // estimated tokens after compaction
}

// MaybeCompact checks if the conversation history needs compaction
// and performs it if so. Returns a CompactResult with the summary,
// kept messages, and whether compaction actually ran.
//
// The algorithm:
//  1. Load existing summary + recent messages
//  2. Estimate total tokens
//  3. If under 75% of budget, do nothing
//  4. If over, take the older half of messages, summarize them
//     (incorporating any existing summary), and store the new summary
//  5. Return the new summary + remaining recent messages
func MaybeCompact(
	chatLLM *llm.Client,
	store *memory.Store,
	conversationID string,
	recentMessages []memory.Message,
	maxHistoryTokens int,
	maxContextTokens int,
	botName, userName string,
) (*CompactResult, error) {
	if maxHistoryTokens <= 0 {
		maxHistoryTokens = 1400 // default — triggers compaction at 75% (~1050 tokens)
	}

	// Load existing summary for this conversation.
	existingSummary, _, err := store.LatestSummary(conversationID)
	if err != nil {
		return nil, fmt.Errorf("loading summary: %w", err)
	}

	// --- Context-aware trigger (#38) ---
	// If we have a total prompt budget, check real prompt utilization instead
	// of just history size. The most recent user message's TokenCount stores
	// the total prompt tokens from the last chat completion call — that
	// includes everything: system prompt, persona, facts, mood, history.
	shouldCompact := false
	if maxContextTokens > 0 {
		// Scan backward for the most recent user message with a real token count.
		var lastPromptTokens int
		for i := len(recentMessages) - 1; i >= 0; i-- {
			if recentMessages[i].Role == "user" && recentMessages[i].TokenCount > 0 {
				lastPromptTokens = recentMessages[i].TokenCount
				break
			}
		}
		if lastPromptTokens > 0 {
			// Same 75% threshold as the estimation path, but applied to the
			// total prompt budget. This accounts for scaffolding overhead
			// (system prompt, persona, facts, mood) that the history-only
			// check can't see.
			threshold := int(float64(maxContextTokens) * 0.75)
			log.Infof("  compaction check (context-aware): %d/%d total prompt tokens (threshold: %d)",
				lastPromptTokens, maxContextTokens, threshold)
			if lastPromptTokens >= threshold {
				shouldCompact = true
			} else {
				return &CompactResult{
					Summary:      existingSummary,
					KeptMessages: recentMessages,
				}, nil
			}
		}
		// If no prompt token data yet (first message), fall through to estimation.
	}

	// --- Estimation-based trigger (#37, improved with real counts) ---
	// Fallback when context_window isn't configured or no prompt token data exists.
	if !shouldCompact {
		estTokens := EstimateHistoryTokens(existingSummary, recentMessages)
		threshold := int(float64(maxHistoryTokens) * 0.75)
		log.Infof("  compaction check (estimated): %d msgs, %d tokens (threshold: %d, budget: %d)",
			len(recentMessages), estTokens, threshold, maxHistoryTokens)
		if estTokens < threshold {
			return &CompactResult{
				Summary:      existingSummary,
				KeptMessages: recentMessages,
			}, nil
		}
		shouldCompact = true
	}

	// Estimate tokens before compaction (for logging and the result struct).
	tokensBefore := EstimateHistoryTokens(existingSummary, recentMessages)
	log.Infof("  compacting: %d messages, ~%d history tokens", len(recentMessages), tokensBefore)

	// Split: older half gets summarized, newer half stays verbatim.
	// We keep at least 6 messages (3 exchanges) in full fidelity so
	// the model doesn't lose immediate context.
	splitPoint := len(recentMessages) / 2
	minKeep := 6
	if len(recentMessages)-splitPoint < minKeep && len(recentMessages) > minKeep {
		splitPoint = len(recentMessages) - minKeep
	}
	if splitPoint <= 0 {
		// Not enough messages to compact.
		return &CompactResult{
			Summary:      existingSummary,
			KeptMessages: recentMessages,
		}, nil
	}

	toSummarize := recentMessages[:splitPoint]
	toKeep := recentMessages[splitPoint:]

	// Build the transcript of messages to summarize.
	var transcript strings.Builder
	if existingSummary != "" {
		fmt.Fprintf(&transcript, "[Summary of earlier conversation:]\n%s\n\n[Continuing from there:]\n\n", existingSummary)
	}
	for _, msg := range toSummarize {
		role := userName
		if msg.Role == "assistant" {
			role = botName
		}
		content := msg.ContentScrubbed
		if content == "" {
			content = msg.ContentRaw
		}
		fmt.Fprintf(&transcript, "%s: %s\n\n", role, content)
	}

	// Ask the LLM to summarize.
	llmMessages := []llm.ChatMessage{
		{Role: "system", Content: fmt.Sprintf(summaryPromptTmpl, userName, botName)},
		{Role: "user", Content: transcript.String()},
	}

	// Guard against nil LLM (happens in tests and if chat model is misconfigured).
	if chatLLM == nil {
		log.Warn("no LLM client available, skipping compaction")
		return &CompactResult{
			Summary:      existingSummary,
			KeptMessages: recentMessages,
		}, nil
	}

	resp, err := chatLLM.ChatCompletion(llmMessages)
	if err != nil {
		// If summarization fails, just return everything unsummarized.
		// Better to have a fat context than lose data.
		log.Warn("summarization failed, skipping compaction", "err", err)
		return &CompactResult{
			Summary:      existingSummary,
			KeptMessages: recentMessages,
		}, nil
	}

	newSummary := resp.Content

	// Store the summary in the DB.
	startID := toSummarize[0].ID
	endID := toSummarize[len(toSummarize)-1].ID
	_, err = store.SaveSummary(conversationID, newSummary, startID, endID)
	if err != nil {
		log.Error("failed to save summary", "err", err)
		return &CompactResult{
			Summary:      existingSummary,
			KeptMessages: recentMessages,
		}, nil
	}

	newTokens := EstimateHistoryTokens(newSummary, toKeep)
	log.Infof("  compacted %d messages (%d→%d tokens, saved %d)",
		len(toSummarize), tokensBefore, newTokens, tokensBefore-newTokens)
	log.Infof("  summary: %s", truncate(newSummary, 200))

	// Log metrics for the summarization call.
	store.SaveMetric(resp.Model, resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, 0, 0)

	return &CompactResult{
		Summary:      newSummary,
		KeptMessages: toKeep,
		DidCompact:   true,
		Summarized:   len(toSummarize),
		TokensBefore: tokensBefore,
		TokensAfter:  newTokens,
	}, nil
}

func truncate(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
