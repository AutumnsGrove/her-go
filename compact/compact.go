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
func EstimateHistoryTokens(summary string, messages []memory.Message) int {
	total := estimateTokens(summary)
	for _, msg := range messages {
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
const summaryPrompt = `You are summarizing an earlier part of a conversation between a user and Mira (an AI companion). Your goal is to capture what matters for continuing the conversation naturally.

Preserve:
- What topics were discussed and any conclusions reached
- Emotional tone and how the conversation felt
- Any commitments, plans, or things either person said they'd do
- Context needed to understand references in later messages
- The general arc of the conversation

Don't preserve:
- Exact wording (paraphrase is fine)
- Greetings and small talk unless they established something important
- Repetitive back-and-forth that can be summarized in one line
- Information already captured in the facts/memories system

Write the summary as a brief narrative, like you're catching up a friend who missed the first part of the conversation. Keep it concise. 2-4 sentences for a short exchange, 4-8 for a longer one.

If there's an existing summary of even earlier conversation, incorporate it naturally into your new summary. Don't just append, weave it together.`

// MaybeCompact checks if the conversation history needs compaction
// and performs it if so. Returns the summary to use (may be empty
// if no summary exists or was needed) and the messages that should
// remain in the context window.
//
// The algorithm:
// 1. Load existing summary + recent messages
// 2. Estimate total tokens
// 3. If under 75% of budget, do nothing
// 4. If over, take the older half of messages, summarize them
//    (incorporating any existing summary), and store the new summary
// 5. Return the new summary + remaining recent messages
func MaybeCompact(
	chatLLM *llm.Client,
	store *memory.Store,
	conversationID string,
	recentMessages []memory.Message,
	maxHistoryTokens int,
) (summary string, keptMessages []memory.Message, err error) {
	if maxHistoryTokens <= 0 {
		maxHistoryTokens = 8000 // default
	}

	// Load existing summary for this conversation.
	existingSummary, _, err := store.LatestSummary(conversationID)
	if err != nil {
		return "", recentMessages, fmt.Errorf("loading summary: %w", err)
	}

	// Estimate current token usage.
	currentTokens := EstimateHistoryTokens(existingSummary, recentMessages)

	// Trigger at 75% of budget.
	threshold := int(float64(maxHistoryTokens) * 0.75)
	if currentTokens < threshold {
		// Under budget, no compaction needed.
		return existingSummary, recentMessages, nil
	}

	log.Infof("  history at %d tokens (threshold: %d), compacting...", currentTokens, threshold)

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
		return existingSummary, recentMessages, nil
	}

	toSummarize := recentMessages[:splitPoint]
	toKeep := recentMessages[splitPoint:]

	// Build the transcript of messages to summarize.
	var transcript strings.Builder
	if existingSummary != "" {
		fmt.Fprintf(&transcript, "[Summary of earlier conversation:]\n%s\n\n[Continuing from there:]\n\n", existingSummary)
	}
	for _, msg := range toSummarize {
		role := "User"
		if msg.Role == "assistant" {
			role = "Mira"
		}
		content := msg.ContentScrubbed
		if content == "" {
			content = msg.ContentRaw
		}
		fmt.Fprintf(&transcript, "%s: %s\n\n", role, content)
	}

	// Ask the LLM to summarize.
	llmMessages := []llm.ChatMessage{
		{Role: "system", Content: summaryPrompt},
		{Role: "user", Content: transcript.String()},
	}

	resp, err := chatLLM.ChatCompletion(llmMessages)
	if err != nil {
		// If summarization fails, just return everything unsummarized.
		// Better to have a fat context than lose data.
		log.Warn("summarization failed, skipping compaction", "err", err)
		return existingSummary, recentMessages, nil
	}

	newSummary := resp.Content

	// Store the summary in the DB.
	startID := toSummarize[0].ID
	endID := toSummarize[len(toSummarize)-1].ID
	_, err = store.SaveSummary(conversationID, newSummary, startID, endID)
	if err != nil {
		log.Error("failed to save summary", "err", err)
		return existingSummary, recentMessages, nil
	}

	newTokens := EstimateHistoryTokens(newSummary, toKeep)
	log.Infof("  compacted %d messages (%d→%d tokens, saved %d)",
		len(toSummarize), currentTokens, newTokens, currentTokens-newTokens)
	log.Infof("  summary: %s", truncate(newSummary, 200))

	// Log metrics for the summarization call.
	store.SaveMetric(resp.Model, resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, 0, 0)

	return newSummary, toKeep, nil
}

func truncate(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
