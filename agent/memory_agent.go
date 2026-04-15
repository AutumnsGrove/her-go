// Package agent — post-turn background memory agent.
//
// After the main agent delivers its reply, RunMemoryAgent runs in a goroutine
// to review the conversation turn and extract facts. The user already has
// their reply before any fact-saving work begins.
//
// This separates two concerns that used to be tangled:
//   - Main agent: orchestrate the turn, reply, done. No memory writes.
//   - Memory agent: read the turn transcript, decide what to save.
//
// The memory agent uses the same tool registry (tools.Execute) and the
// same fact pipeline (style gate, length gate, dedup, classifier) as the
// old inline save_fact calls — just without the time pressure.
package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"her/config"
	"her/embed"
	"her/llm"
	"her/memory"
	"her/tools"

	// Blank imports register the memory tool handlers in tools.Execute's
	// dispatch table. Same pattern as agent.go's blank imports — the init()
	// in each package calls tools.Register(name, handler).
	_ "her/tools/done"
	_ "her/tools/remove_fact"
	_ "her/tools/save_fact"
	_ "her/tools/save_self_fact"
	_ "her/tools/update_fact"
)

// MemoryAgentInput is the turn transcript passed from the main agent loop
// after the reply has been sent. Contains everything the memory agent needs
// to decide what to save.
type MemoryAgentInput struct {
	UserMessage    string   // scrubbed user message
	ThinkTraces    []string // contents of every think() call made by the main agent
	ReplyText      string   // the text actually sent to the user
	TriggerMsgID   int64    // message ID that triggered this turn
	ConversationID string
}

// MemoryAgentParams bundles the dependencies the memory agent needs.
// Smaller than RunParams — no callbacks, no TUI, no Telegram.
type MemoryAgentParams struct {
	LLM           *llm.Client    // nil = memory agent disabled
	ClassifierLLM *llm.Client    // nil = classifier disabled (writes pass through)
	Store         *memory.Store
	EmbedClient   *embed.Client
	Cfg           *config.Config
}

// defaultMemoryAgentPrompt is used when memory_agent_prompt.md can't be loaded.
const defaultMemoryAgentPrompt = `You are {{her}}'s memory curator. Review this conversation turn and decide what facts are worth saving permanently.

Use save_fact for facts about {{user}}.
Use save_self_fact for observations about {{her}}'s own patterns, communication style, or relationship dynamics.
Use update_fact when something you already know has changed or been refined.
Use remove_fact for facts that are now incorrect or made redundant by new information.

Rules for writing good facts:
- Write facts as timeless truths — NO temporal references like "today", "last week", or "right now"
- Only save what would matter in a conversation 30 days from now
- Be specific: "{{user}} prefers stealth builds in Elden Ring" beats "{{user}} likes games"
- User preferences ABOUT fiction are real facts. In-game events are NOT.
- Transient moods (tired today, stressed this week) are NOT facts — skip them.
- Do NOT re-save anything already in the existing facts list.

Call done when finished.`

// RunMemoryAgent reviews the given turn transcript and saves any facts worth
// keeping. Runs a lightweight tool-calling loop (max 10 iterations) using
// the memory tools: save_fact, save_self_fact, update_fact, remove_fact, done.
//
// This function is designed to be called inside a goroutine — it logs
// results but never returns an error to the caller. A missing fact is
// acceptable; a crash in the background is not.
func RunMemoryAgent(input MemoryAgentInput, params MemoryAgentParams) {
	if params.LLM == nil {
		return
	}

	log.Info("─── memory agent ───")

	// Load the memory agent prompt (hot-reloadable like other prompt files).
	promptContent := loadMemoryAgentPrompt(params.Cfg)

	// Build the turn transcript the model will review.
	transcript := buildMemoryTranscript(input, params.Store)

	// Pre-approved rewrites: shared between ClassifyWriteFunc (which populates it)
	// and the tool handlers (which check it). Same pattern as the main agent.
	preApproved := make(map[string]bool)

	// Build a minimal tools.Context — only the fields memory tools actually use.
	// No callbacks, no TUI bus, no scrub vault.
	tctx := &tools.Context{
		Store:               params.Store,
		EmbedClient:         params.EmbedClient,
		SimilarityThreshold: params.Cfg.Embed.SimilarityThreshold,
		ClassifierLLM:       params.ClassifierLLM,
		Cfg:                 params.Cfg,
		ConversationID:      input.ConversationID,
		TriggerMsgID:        input.TriggerMsgID,
		PreApprovedRewrites: preApproved,
		// ClassifyWriteFunc and RejectionMessageFunc are injected below
		// to avoid circular imports — same pattern as the main agent.
		ClassifyWriteFunc: func(writeType, content string, snippet []memory.Message) tools.ClassifyVerdict {
			verdict := classifyMemoryWrite(params.ClassifierLLM, writeType, content, snippet)
			// Log to classifier_log for observability.
			if err := params.Store.SaveClassifierLog(
				input.ConversationID, writeType, verdict.Type, content, verdict.Reason, verdict.Rewrite,
			); err != nil {
				log.Error("memory agent: saving classifier log", "err", err)
			}
			if verdict.Rewrite != "" {
				preApproved[strings.ToLower(verdict.Rewrite)] = true
			}
			return verdict
		},
		RejectionMessageFunc: func(verdict tools.ClassifyVerdict) string {
			return rejectionMessage(verdict)
		},
	}

	// Tool definitions for the memory agent — the 4 memory tools plus done.
	// These are loaded from the same YAML registry as all other tools.
	memToolDefs := tools.LookupToolDefs(
		[]string{"save_fact", "save_self_fact", "update_fact", "remove_fact", "done"},
		params.Cfg,
	)

	messages := []llm.ChatMessage{
		{Role: "system", Content: promptContent},
		{Role: "user", Content: transcript},
	}

	var totalCost float64
	const maxIterations = 10

	for i := 0; i < maxIterations; i++ {
		resp, err := params.LLM.ChatCompletionWithTools(messages, memToolDefs)
		if err != nil {
			log.Error("memory agent: LLM error", "err", err)
			break
		}

		// Log cost and metrics — same as main agent.
		params.Store.SaveMetric(resp.Model, resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, 0, input.TriggerMsgID)
		totalCost += resp.CostUSD
		log.Infof("  [memory] tokens: %d prompt + %d completion | $%.6f | finish=%s",
			resp.PromptTokens, resp.CompletionTokens, resp.CostUSD, resp.FinishReason)

		if len(resp.ToolCalls) == 0 {
			// Model returned text or an empty response — stop the loop.
			break
		}

		// Append the assistant turn before executing tools.
		messages = append(messages, llm.ChatMessage{
			Role:      "assistant",
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})

		// Execute each tool call.
		for _, tc := range resp.ToolCalls {
			result := tools.Execute(tc.Function.Name, tc.Function.Arguments, tctx)
			log.Infof("    [memory] %s → %s", tc.Function.Name, truncateLog(result, 150))
			messages = append(messages, llm.ChatMessage{
				Role:       "tool",
				Content:    result,
				ToolCallID: tc.ID,
			})
		}

		if tctx.DoneCalled {
			break
		}
	}

	log.Infof("  memory agent: %d facts saved | $%.6f", len(tctx.SavedFacts), totalCost)
}

// loadMemoryAgentPrompt reads memory_agent_prompt.md from the same directory
// as prompt.md, falling back to the hardcoded default if the file is missing.
func loadMemoryAgentPrompt(cfg *config.Config) string {
	dir := filepath.Dir(cfg.Persona.PromptFile)
	promptPath := filepath.Join(dir, "memory_agent_prompt.md")
	data, err := os.ReadFile(promptPath)
	if err != nil || len(data) == 0 {
		return cfg.ExpandPrompt(defaultMemoryAgentPrompt)
	}
	return cfg.ExpandPrompt(string(data))
}

// buildMemoryTranscript formats the turn's key events into a structured
// transcript that the memory model can parse. The format is intentionally
// simple — three labelled sections that map directly to what the model
// needs to make save/skip decisions.
func buildMemoryTranscript(input MemoryAgentInput, store *memory.Store) string {
	var b strings.Builder

	b.WriteString("## What the user said\n")
	b.WriteString(input.UserMessage)
	b.WriteString("\n\n")

	if len(input.ThinkTraces) > 0 {
		b.WriteString("## Agent's reasoning this turn\n")
		for _, trace := range input.ThinkTraces {
			b.WriteString(trace)
			b.WriteString("\n")
		}
		b.WriteString("\n")
	}

	if input.ReplyText != "" {
		b.WriteString("## Reply sent to user\n")
		b.WriteString(input.ReplyText)
		b.WriteString("\n\n")
	}

	// Show existing facts for dedup context — the model should not re-save
	// anything already here. Cap at 30 to avoid flooding the context.
	facts, err := store.AllActiveFacts()
	if err == nil && len(facts) > 0 {
		b.WriteString("## Existing facts (do NOT re-save these)\n")
		limit := 30
		if len(facts) < limit {
			limit = len(facts)
		}
		for _, f := range facts[:limit] {
			fmt.Fprintf(&b, "- [ID=%d] %s\n", f.ID, f.Fact)
		}
	}

	return b.String()
}
