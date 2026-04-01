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
	botName, userName string,
) (*CompactResult, error) {
	if maxHistoryTokens <= 0 {
		maxHistoryTokens = 3000 // default — triggers compaction at 75% (~2250 tokens)
	}

	// Load existing summary for this conversation.
	existingSummary, _, err := store.LatestSummary(conversationID, "chat")
	if err != nil {
		return nil, fmt.Errorf("loading summary: %w", err)
	}

	// Two independent compaction triggers. Either one can fire compaction:
	//
	// 1. Context-aware: checks the history-only token count from the
	//    last chat model call (stored on user messages by execReply,
	//    with scaffolding subtracted). Uses real API token counts.
	//
	// 2. Estimation-based: checks the compaction window's estimated
	//    tokens against the same history budget using len/4 heuristic.
	//    Catches unsummarized history accumulating in the DB.
	//
	// Both triggers compare against maxHistoryTokens — the budget for
	// conversation history that compaction can actually shrink.

	shouldCompact := false

	// --- Context-aware trigger (real history tokens) ---
	{
		var lastHistoryTokens int
		for i := len(recentMessages) - 1; i >= 0; i-- {
			if recentMessages[i].Role == "user" && recentMessages[i].TokenCount > 0 {
				lastHistoryTokens = recentMessages[i].TokenCount
				break
			}
		}
		if lastHistoryTokens > 0 {
			threshold := int(float64(maxHistoryTokens) * 0.75)
			log.Infof("  compaction check (real history): %d/%d tokens (threshold: %d)",
				lastHistoryTokens, maxHistoryTokens, threshold)
			if lastHistoryTokens >= threshold {
				shouldCompact = true
			}
		}
		// No early return — always continue to the estimation check.
	}

	// --- Estimation-based trigger ---
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
	}

	// Estimate tokens before compaction (for logging and the result struct).
	tokensBefore := EstimateHistoryTokens(existingSummary, recentMessages)
	log.Infof("  compacting: %d messages, ~%d history tokens", len(recentMessages), tokensBefore)

	// Split: keep only the most recent messages in full fidelity,
	// summarize everything else. We keep 6 messages (3 exchanges) —
	// enough for the model to resolve references like "it", "that
	// thing", etc. Everything older goes into the running summary.
	minKeep := 6
	if len(recentMessages) <= minKeep {
		// Not enough messages to compact.
		return &CompactResult{
			Summary:      existingSummary,
			KeptMessages: recentMessages,
		}, nil
	}
	splitPoint := len(recentMessages) - minKeep

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
	_, err = store.SaveSummary(conversationID, newSummary, startID, endID, "chat")
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

// verboseTools is a package-level alias for VerboseTools, used by the
// agent compaction logic below. The canonical list lives in verbose_tools.go.
var verboseTools = VerboseTools

// agentSummaryPromptTmpl is the prompt for summarizing the agent's action
// history. Unlike the chat summary (conversational flow), this focuses on
// what the agent DID — tools called, decisions made, outcomes achieved.
// %s placeholders: botName.
const agentSummaryPromptTmpl = `You are summarizing the action history of %s's agent system — the tool-calling orchestrator that runs behind the scenes.

Preserve:
- Which tools were called and why (save_fact, update_fact, remove_fact, create_reminder, set_location, etc.)
- What facts were saved, updated, or removed (include fact IDs when available)
- Decisions made: why the agent chose one action over another
- Outcomes: did the tool call succeed or fail? What was the result?
- Any patterns: repeated searches, fact corrections, reminder chains

Don't preserve:
- Raw search results (web_search, book_search output) — just note what was searched and if useful results were found
- Tool discovery (find_skill) — just note which tools were activated
- Exact JSON arguments — paraphrase the intent
- Think tool internal monologue — summarize the conclusion only

Write the summary as a concise action log. Use brief, factual statements. Example:
"Saved fact #42 about user's job (software engineer). Searched web for Go testing patterns — found useful results. Set reminder for medication at 9pm daily. Updated fact #15 (corrected user's timezone from EST to PST)."

If there's an existing summary of earlier actions, incorporate it naturally.`

// AgentCompactResult holds the output of MaybeCompactAgent.
type AgentCompactResult struct {
	Summary      string            // running agent action summary
	RecentActions []memory.AgentAction // actions kept in full fidelity
	DidCompact   bool              // true if summarization ran this call
	Summarized   int               // number of actions summarized
	TokensBefore int               // estimated tokens before
	TokensAfter  int               // estimated tokens after
}

// estimateActionTokens estimates the token count for a set of agent actions
// plus an existing summary. Verbose tool results are counted at their
// truncated size since that's what actually goes into the prompt.
func estimateActionTokens(summary string, actions []memory.AgentAction) int {
	total := estimateTokens(summary)
	for _, a := range actions {
		total += estimateTokens(a.ToolName) + 5 // tool name + formatting
		total += estimateTokens(a.ToolArgs)
		if verboseTools[a.ToolName] {
			// Verbose tools get truncated to ~200 chars in the prompt
			if len(a.Result) > 200 {
				total += 50 // ~200 chars / 4
			} else {
				total += estimateTokens(a.Result)
			}
		} else {
			total += estimateTokens(a.Result)
		}
		total += 10 // overhead for formatting
	}
	return total
}

// formatActionTranscript builds the text that gets sent to the LLM for
// summarization. Verbose tool results are truncated to save prompt tokens.
func formatActionTranscript(existingSummary string, actions []memory.AgentAction) string {
	var b strings.Builder
	if existingSummary != "" {
		fmt.Fprintf(&b, "[Summary of earlier agent actions:]\n%s\n\n[Actions since then:]\n\n", existingSummary)
	}
	for _, a := range actions {
		result := a.Result
		if verboseTools[a.ToolName] && len(result) > 200 {
			result = result[:200] + "... (truncated)"
		}
		fmt.Fprintf(&b, "→ %s(%s)\n  Result: %s\n\n", a.ToolName, a.ToolArgs, result)
	}
	return b.String()
}

// MaybeCompactAgent checks if the agent's action history needs compaction
// and performs it if so. This is the agent-side counterpart to MaybeCompact.
//
// Instead of summarizing conversation messages, it summarizes the agent's
// tool call history (from agent_turns). The summary preserves what the
// agent DID so it can build on past decisions, update previous facts, etc.
func MaybeCompactAgent(
	chatLLM *llm.Client,
	store *memory.Store,
	conversationID string,
	actions []memory.AgentAction,
	agentContextBudget int,
	botName string,
) (*AgentCompactResult, error) {
	if agentContextBudget <= 0 {
		agentContextBudget = 16000 // default
	}

	// Load existing agent summary.
	existingSummary, _, err := store.LatestSummary(conversationID, "agent")
	if err != nil {
		return nil, fmt.Errorf("loading agent summary: %w", err)
	}

	// Check if we need to compact.
	estTokens := estimateActionTokens(existingSummary, actions)
	threshold := int(float64(agentContextBudget) * 0.75)
	log.Infof("  agent compaction check: %d actions, ~%d tokens (threshold: %d, budget: %d)",
		len(actions), estTokens, threshold, agentContextBudget)

	if estTokens < threshold {
		return &AgentCompactResult{
			Summary:       existingSummary,
			RecentActions: actions,
		}, nil
	}

	tokensBefore := estTokens
	log.Infof("  compacting agent actions: %d actions, ~%d tokens", len(actions), tokensBefore)

	// Keep the most recent actions in full fidelity, summarize the rest.
	// We keep more actions than chat messages because actions are smaller
	// and the agent benefits from seeing its recent tool call chain.
	minKeep := 10
	if len(actions) <= minKeep {
		return &AgentCompactResult{
			Summary:       existingSummary,
			RecentActions: actions,
		}, nil
	}
	splitPoint := len(actions) - minKeep

	toSummarize := actions[:splitPoint]
	toKeep := actions[splitPoint:]

	transcript := formatActionTranscript(existingSummary, toSummarize)

	if chatLLM == nil {
		log.Warn("no LLM client available, skipping agent compaction")
		return &AgentCompactResult{
			Summary:       existingSummary,
			RecentActions: actions,
		}, nil
	}

	llmMessages := []llm.ChatMessage{
		{Role: "system", Content: fmt.Sprintf(agentSummaryPromptTmpl, botName)},
		{Role: "user", Content: transcript},
	}

	resp, err := chatLLM.ChatCompletion(llmMessages)
	if err != nil {
		log.Warn("agent summarization failed, skipping compaction", "err", err)
		return &AgentCompactResult{
			Summary:       existingSummary,
			RecentActions: actions,
		}, nil
	}

	newSummary := resp.Content

	// Store with stream="agent". We use the first/last action's message IDs
	// as the range markers (same concept as chat compaction).
	startID := toSummarize[0].MessageID
	endID := toSummarize[len(toSummarize)-1].MessageID
	_, err = store.SaveSummary(conversationID, newSummary, startID, endID, "agent")
	if err != nil {
		log.Error("failed to save agent summary", "err", err)
		return &AgentCompactResult{
			Summary:       existingSummary,
			RecentActions: actions,
		}, nil
	}

	newTokens := estimateActionTokens(newSummary, toKeep)
	log.Infof("  agent compacted %d actions (%d→%d tokens, saved %d)",
		len(toSummarize), tokensBefore, newTokens, tokensBefore-newTokens)
	log.Infof("  agent summary: %s", truncate(newSummary, 200))

	store.SaveMetric(resp.Model, resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, 0, 0)

	return &AgentCompactResult{
		Summary:       newSummary,
		RecentActions: toKeep,
		DidCompact:    true,
		Summarized:    len(toSummarize),
		TokensBefore:  tokensBefore,
		TokensAfter:   newTokens,
	}, nil
}

func truncate(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
