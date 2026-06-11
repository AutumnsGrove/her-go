// Package agent — post-turn background memory agent.
//
// After the driver agent delivers its reply, RunMemoryAgent runs in a goroutine
// to review the conversation turn and extract facts. The user already has
// their reply before any fact-saving work begins.
//
// This separates two concerns that used to be tangled:
//   - Driver agent: orchestrate the turn, reply, done. No memory writes.
//   - Memory agent: read the turn transcript, decide what to save.
//
// The memory agent uses the same tool registry (tools.Execute) and the
// same fact pipeline (style gate, length gate, dedup, classifier) as the
// old inline save_memory calls — just without the time pressure.
package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	engine "her/agent_engine"
	"her/config"
	"her/embed"
	"her/llm"
	"her/memory"
	"her/tools"
	"her/tui"
	"her/turn"

	// Blank imports register the memory tool handlers in tools.Execute's
	// dispatch table. Same pattern as agent.go's blank imports — the init()
	// in each package calls tools.Register(name, handler).
	_ "her/tools/create_card"
	_ "her/tools/done"
	_ "her/tools/list_cards"
	_ "her/tools/notify_agent"
	_ "her/tools/recall_memories"
	_ "her/tools/remove_memory"
	_ "her/tools/save_memory"
	_ "her/tools/save_self_memory"
	_ "her/tools/split_memory"
	_ "her/tools/update_memory"
)

// MemoryAgentInput is the turn transcript passed from the driver agent loop
// after the reply has been sent. Contains everything the memory agent needs
// to decide what to save.
type MemoryAgentInput struct {
	UserMessage    string   // scrubbed user message
	ThinkTraces    []string // contents of every think() call made by the driver agent
	ReplyText      string   // the text actually sent to the user
	TriggerMsgID   int64    // message ID that triggered this turn
	ConversationID string
}

// MemoryAgentParams bundles the dependencies the memory agent needs.
// Smaller than RunParams — no callbacks, no Telegram.
type MemoryAgentParams struct {
	LLM           *llm.Client    // nil = memory agent disabled
	ClassifierLLM *llm.Client    // nil = classifier disabled (writes pass through)
	Store         memory.Store
	EmbedClient   *embed.Client
	Cfg           *config.Config
	TraceCallback tools.TraceCallback    // nil = tracing disabled for memory agent
	EventBus      *tui.Bus               // nil-safe — emits tool call events for the TUI
	AgentEventCB  tools.AgentEventCallback // nil-safe — fires when notify_agent is called
	Phase         *turn.PhaseHandle       // nil-safe — routes events to the parent turn's memory group
}

// defaultMemoryAgentPrompt is used when memory_agent_prompt.md can't be loaded.
const defaultMemoryAgentPrompt = `You are {{her}}'s memory curator. Review this conversation turn and decide what memories are worth saving to the card system.

Memories are organized into topic cards (folders). Call list_cards first to see the landscape.

Workflow: list_cards → pick the right card → recall_memories with card_slug to check for duplicates → save_memory or update_memory.

Tools: list_cards, recall_memories (optional card_slug for scoped search), save_memory (requires card_slug), save_self_memory (requires card_slug), update_memory, remove_memory, split_memory, create_card, notify_agent, done.

Rules:
- Every save_memory/save_self_memory MUST specify a card_slug
- Always recall within the target card before saving to check for duplicates
- Write memories as timeless truths — no "today", "last week", "right now"
- Only save what would matter 30 days from now
- Be specific: "{{user}} prefers stealth builds in Elden Ring" beats "{{user}} likes games"
- Self-memories capture identity, not techniques. "I'm drawn to cosmic imagery" = save. "I used a cosmic metaphor" = reject.

If you see an Inbox section, handle those tasks and call notify_agent when done.
Otherwise call done when finished.`

// RunMemoryAgent reviews the given turn transcript and saves any facts worth
// keeping. Runs a tool-calling loop with continuation windows (15 iterations
// per window, up to 3 windows = 45 calls max) using the memory tools:
// save_memory, save_self_memory, update_memory, remove_memory, split_memory,
// notify_agent, done.
//
// This function is designed to be called inside a goroutine — it logs
// results but never returns an error to the caller. A missing fact is
// acceptable; a crash in the background is not.
// MemoryAgentResult holds the outcome of a memory agent run.
type MemoryAgentResult struct {
	MemoriesSaved int
	Cost          float64
}

func RunMemoryAgent(input MemoryAgentInput, params MemoryAgentParams) MemoryAgentResult {
	if params.LLM == nil {
		return MemoryAgentResult{}
	}

	log.Info("─── memory agent ───")

	// Capture the conversation snapshot NOW — before any subsequent turn can
	// write new messages to the DB. If we query lazily inside each tool call,
	// the next turn's messages may already be present and the classifier will
	// see the wrong context (it would reject "user built a grocery tool" because
	// the snippet shows "user asked about cortisol research" from the next turn).
	contextSnippet, _ := params.Store.RecentMessages(input.ConversationID, 2)

	// Pre-approved rewrites: shared between ClassifyWriteFunc (which populates it)
	// and the tool handlers (which check it). Same pattern as the driver agent.
	preApproved := make(map[string]bool)

	// Build a minimal tools.Context — only the fields memory tools actually use.
	// No callbacks, no TUI bus, no scrub vault.
	// ClassifierSnippet carries the pre-captured context snapshot so
	// fact_helpers doesn't re-query the DB (which may have newer messages
	// from subsequent turns by the time classification runs).
	tctx := &tools.Context{
		AgentName:           "memory",
		Store:               params.Store,
		EmbedClient:         params.EmbedClient,
		SimilarityThreshold: params.Cfg.Embed.SimilarityThreshold,
		ClassifierLLM:       params.ClassifierLLM,
		Cfg:                 params.Cfg,
		ConversationID:      input.ConversationID,
		TriggerMsgID:        input.TriggerMsgID,
		PreApprovedRewrites: preApproved,
		ClassifierSnippet:   contextSnippet,
		AgentEventCB:        params.AgentEventCB,
	}

	// Load the memory agent prompt (hot-reloadable like other prompt files).
	promptContent := loadMemoryAgentPrompt(params.Cfg)
	transcript := buildMemoryTranscript(input, params.Store)

	// Track consecutive rejections to prevent the agent from burning
	// iterations retrying the same memory save. After 5 consecutive
	// rejections, the hook rewrites the result to tell the agent to
	// move on. Resets when a tool call succeeds or a different tool runs.
	const maxConsecutiveRejections = 5
	consecutiveRejections := 0

	// Run the tool-calling loop via the shared engine.
	loopResult, err := engine.RunLoop(engine.EngineConfig{
		Name:                "memory",
		MetricRole:          memory.RoleMemory,
		LLM:                 params.LLM,
		Store:               params.Store,
		ToolDefs:            tools.ToolDefsForAgent("memory", params.Cfg),
		ToolCtx:             tctx,
		TriggerMsgID:        input.TriggerMsgID,
		IterationsPerWindow: params.Cfg.MemoryAgent.IterationsPerWindow,
		MaxContinuations:    params.Cfg.MemoryAgent.MaxContinuations,
		TraceCallback:       params.TraceCallback,
		EventBus:            params.EventBus,
		Phase:               params.Phase,
		Messages: []llm.ChatMessage{
			{Role: "system", Content: promptContent},
			{Role: "user", Content: transcript},
		},

		ContinuationMsg: func(window, maxWindows int, summary string) string {
			return fmt.Sprintf(
				"You have used all iterations in the previous window without calling done. "+
					"Continuation window %d of %d. Your progress so far:\n%s\n\n"+
					"Continue your work and call done (or notify_agent) when finished.",
				window, maxWindows, summary,
			)
		},

		// PostToolResult: enforce a rejection limit so the agent doesn't
		// spiral trying to rewrite the same memory 15 times.
		PostToolResult: func(tc llm.ToolCall, result string, isError bool) string {
			isMemoryWrite := tc.Function.Name == "save_memory" ||
				tc.Function.Name == "save_self_memory" ||
				tc.Function.Name == "update_memory"

			if !isMemoryWrite {
				consecutiveRejections = 0
				return result
			}

			if strings.HasPrefix(result, "rejected:") {
				consecutiveRejections++
				if consecutiveRejections >= maxConsecutiveRejections {
					log.Warn("memory agent: rejection limit reached, forcing move-on",
						"consecutive", consecutiveRejections, "tool", tc.Function.Name)
					return fmt.Sprintf(
						"rejected: you have been rejected %d times in a row for this memory. "+
							"STOP trying to save this particular observation — the classifier "+
							"will not accept it. Move on to other memories or call done.",
						consecutiveRejections)
				}
			} else {
				consecutiveRejections = 0
			}
			return result
		},
	})
	if err != nil {
		log.Error("memory agent: engine error", "err", err)
	}

	totalCost := 0.0
	if loopResult != nil {
		totalCost = loopResult.TotalCost
	}

	log.Infof("  memory agent: %d memories saved | $%.6f", len(tctx.SavedMemories), totalCost)

	if params.Phase != nil {
		params.Phase.Done(turn.PhaseMetrics{
			Cost:          totalCost,
			MemoriesSaved: len(tctx.SavedMemories),
		})
	}

	return MemoryAgentResult{
		MemoriesSaved: len(tctx.SavedMemories),
		Cost:          totalCost,
	}
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
func buildMemoryTranscript(input MemoryAgentInput, store memory.Store) string {
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

	// Check the inbox for tasks delegated by the driver agent (via send_task).
	// Consumed atomically — once read here, they won't appear again.
	inboxMsgs, err := store.ConsumeInbox("memory")
	if err == nil && len(inboxMsgs) > 0 {
		b.WriteString("## Inbox — tasks from the driver agent\n")
		b.WriteString("The driver agent has delegated these tasks to you. Handle them alongside your normal memory work.\n\n")
		for _, msg := range inboxMsgs {
			fmt.Fprintf(&b, "### Task: %s\n%s\n\n", msg.MsgType, msg.Payload)
		}
	}

	// Inject the card landscape so the agent knows where to file memories
	// without needing to call list_cards (saves a full LLM round-trip per turn).
	allCards, cardErr := store.AllCards()
	if cardErr == nil && len(allCards) > 0 {
		b.WriteString("## Card landscape\n")
		for _, c := range allCards {
			summary := c.Summary
			if summary == "" {
				summary = "(no summary yet)"
			}
			fmt.Fprintf(&b, "- **%s** [%s] %s\n", c.TopicSlug, c.Subject, summary)
		}
		b.WriteString("\nUse recall_memories with card_slug to check for duplicates before saving.\n")
	}

	return b.String()
}
