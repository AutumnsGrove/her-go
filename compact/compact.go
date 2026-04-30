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
	_ "embed"
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

// summaryPromptTmpl is loaded from summary_prompt.md.
// Parameters (in order): userName, botName, userName, userName.
//
//go:embed summary_prompt.md
var summaryPromptTmpl string

// CompactResult holds the output of MaybeCompact so callers can tell
// whether compaction actually ran (vs. just returning existing state).
type CompactResult struct {
	Summary      string           // running summary (may be empty if no history)
	KeptMessages []memory.Message // messages that should stay in full fidelity
	Triggered    bool             // true if the threshold check decided compaction was needed (set even if downstream summarization later failed or was skipped)
	DidCompact   bool             // true if new summarization was actually performed this call
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
	store memory.Store,
	conversationID string,
	recentMessages []memory.Message,
	maxHistoryTokens int,
	botName, userName string,
) (*CompactResult, error) {
	if maxHistoryTokens <= 0 {
		maxHistoryTokens = 8000 // default — triggers compaction at 75% (~6000 tokens)
	}

	// Load existing summary for this conversation.
	// summaryEndID is the ID of the last message that was already summarized.
	// Messages with id <= summaryEndID are captured in the summary and must
	// not be counted toward the token estimate — doing so causes runaway
	// re-compaction (the estimator sees the full DB history every turn and
	// thinks history is huge even after compaction).
	existingSummary, summaryEndID, err := store.LatestSummary(conversationID, "chat")
	if err != nil {
		return nil, fmt.Errorf("loading summary: %w", err)
	}

	// Filter to only messages that haven't been summarized yet. These are
	// the only messages that will actually appear in the chat model's prompt,
	// so they're the only ones that count toward the token budget.
	//
	// This replaces the former "real signal wins over estimation" dual-signal
	// logic. By filtering on end_id rather than on stored TokenCount values,
	// the estimator is always accurate without any special-casing.
	unsummarized := recentMessages
	if summaryEndID > 0 {
		filtered := recentMessages[:0]
		for _, msg := range recentMessages {
			if msg.ID > summaryEndID {
				filtered = append(filtered, msg)
			}
		}
		unsummarized = filtered
	}

	threshold := int(float64(maxHistoryTokens) * compactionThreshold)
	estTokens := EstimateHistoryTokens(existingSummary, unsummarized)
	log.Infof("  compaction check: %d un-summarized msgs, ~%d tokens (threshold: %d, budget: %d)",
		len(unsummarized), estTokens, threshold, maxHistoryTokens)
	shouldCompact := estTokens >= threshold

	if !shouldCompact {
		return &CompactResult{
			Summary:      existingSummary,
			KeptMessages: recentMessages,
		}, nil
	}

	// Past this point, the trigger has fired. Triggered=true is set on
	// every CompactResult returned from here on, so callers (and tests)
	// can distinguish "trigger decided we needed to compact" from "actual
	// summarization happened" — DidCompact stays the source of truth for
	// the second question.

	// Estimate tokens before compaction (for logging and the result struct).
	tokensBefore := EstimateHistoryTokens(existingSummary, unsummarized)
	log.Infof("  compacting: %d un-summarized messages, ~%d history tokens", len(unsummarized), tokensBefore)

	// Split: keep only the most recent un-summarized messages in full fidelity,
	// summarize everything else. We keep 6 messages (3 exchanges) —
	// enough for the model to resolve references like "it", "that
	// thing", etc. Everything older goes into the running summary.
	minKeep := 6
	if len(unsummarized) <= minKeep {
		// Not enough un-summarized messages to compact.
		return &CompactResult{
			Summary:      existingSummary,
			KeptMessages: recentMessages,
			Triggered:    true,
		}, nil
	}
	splitPoint := len(unsummarized) - minKeep

	toSummarize := unsummarized[:splitPoint]
	toKeep := unsummarized[splitPoint:]

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
		{Role: "system", Content: fmt.Sprintf(summaryPromptTmpl, userName, botName, userName, userName)},
		{Role: "user", Content: transcript.String()},
	}

	// Guard against nil LLM (happens in tests and if chat model is misconfigured).
	if chatLLM == nil {
		log.Warn("no LLM client available, skipping compaction")
		return &CompactResult{
			Summary:      existingSummary,
			KeptMessages: recentMessages,
			Triggered:    true,
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
			Triggered:    true,
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
			Triggered:    true,
		}, nil
	}

	newTokens := EstimateHistoryTokens(newSummary, toKeep)
	log.Infof("  compacted %d messages (%d→%d tokens, saved %d)",
		len(toSummarize), tokensBefore, newTokens, tokensBefore-newTokens)
	log.Infof("  summary: %s", truncate(newSummary, 200))

	// Log metrics for the summarization call.
	store.SaveMetric(resp.Model, resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, 0, 0, resp.UsedFallback)

	return &CompactResult{
		Summary:      newSummary,
		KeptMessages: toKeep,
		Triggered:    true,
		DidCompact:   true,
		Summarized:   len(toSummarize),
		TokensBefore: tokensBefore,
		TokensAfter:  newTokens,
	}, nil
}

// compactionThreshold is the fraction of the token budget at which
// compaction fires. Both chat and agent history use the same threshold.
// Lives here (not in config) because it's an architectural invariant,
// not a per-deployment tunable.
const compactionThreshold = 0.75

// verboseTools is a package-level alias for VerboseTools, used by the
// agent compaction logic below. The canonical list lives in verbose_tools.go.
var verboseTools = VerboseTools

// agentSummaryPromptTmpl is loaded from agent_summary_prompt.md.
// Parameters (in order): botName.
//
//go:embed agent_summary_prompt.md
var agentSummaryPromptTmpl string

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
	store memory.Store,
	conversationID string,
	actions []memory.AgentAction,
	agentContextBudget int,
	botName string,
) (*AgentCompactResult, error) {
	if agentContextBudget <= 0 {
		agentContextBudget = 16000 // default
	}

	// Load existing driver summary.
	existingSummary, _, err := store.LatestSummary(conversationID, "driver")
	if err != nil {
		return nil, fmt.Errorf("loading driver summary: %w", err)
	}

	// Check if we need to compact.
	estTokens := estimateActionTokens(existingSummary, actions)
	threshold := int(float64(agentContextBudget) * compactionThreshold)
	log.Infof("  driver compaction check: %d actions, ~%d tokens (threshold: %d, budget: %d)",
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

	// Store with stream="driver". We use the first/last action's message IDs
	// as the range markers (same concept as chat compaction).
	startID := toSummarize[0].MessageID
	endID := toSummarize[len(toSummarize)-1].MessageID
	_, err = store.SaveSummary(conversationID, newSummary, startID, endID, "driver")
	if err != nil {
		log.Error("failed to save driver summary", "err", err)
		return &AgentCompactResult{
			Summary:       existingSummary,
			RecentActions: actions,
		}, nil
	}

	newTokens := estimateActionTokens(newSummary, toKeep)
	log.Infof("  agent compacted %d actions (%d→%d tokens, saved %d)",
		len(toSummarize), tokensBefore, newTokens, tokensBefore-newTokens)
	log.Infof("  agent summary: %s", truncate(newSummary, 200))

	store.SaveMetric(resp.Model, resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, 0, 0, resp.UsedFallback)

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
