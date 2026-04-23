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
// old inline save_memory calls — just without the time pressure.
package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

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
	_ "her/tools/done"
	_ "her/tools/notify_agent"
	_ "her/tools/remove_memory"
	_ "her/tools/save_memory"
	_ "her/tools/save_self_memory"
	_ "her/tools/split_memory"
	_ "her/tools/update_memory"
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
// Smaller than RunParams — no callbacks, no Telegram.
type MemoryAgentParams struct {
	LLM           *llm.Client    // nil = memory agent disabled
	ClassifierLLM *llm.Client    // nil = classifier disabled (writes pass through)
	Store         *memory.Store
	EmbedClient   *embed.Client
	Cfg           *config.Config
	TraceCallback tools.TraceCallback    // nil = tracing disabled for memory agent
	EventBus      *tui.Bus               // nil-safe — emits tool call events for the TUI
	AgentEventCB  tools.AgentEventCallback // nil-safe — fires when notify_agent is called
	Phase         *turn.PhaseHandle       // nil-safe — routes events to the parent turn's memory group
}

// defaultMemoryAgentPrompt is used when memory_agent_prompt.md can't be loaded.
const defaultMemoryAgentPrompt = `You are {{her}}'s memory curator. Review this conversation turn and decide what memories are worth saving permanently.

Use save_memory for memories about {{user}}.
Use save_self_memory for observations about {{her}}'s own patterns, communication style, or relationship dynamics.
Use update_memory when something you already know has changed or been refined.
Use remove_memory for memories that are now incorrect or made redundant by new information. Accepts memory_id (single) or memory_ids (batch).
Use split_memory to break a compound memory into individual facts.
Use notify_agent (instead of done) when you completed inbox tasks and the user should be told.

Rules for writing good memories:
- Write memories as timeless truths — NO temporal references like "today", "last week", or "right now"
- Only save what would matter in a conversation 30 days from now
- Be specific: "{{user}} prefers stealth builds in Elden Ring" beats "{{user}} likes games"
- User preferences ABOUT fiction are real memories. In-game events are NOT.
- Transient moods (tired today, stressed this week) are NOT memories — skip them.
- Do NOT re-save anything already in the existing memories list.

If you see an Inbox section in your transcript, handle those tasks and call notify_agent when done.
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
func RunMemoryAgent(input MemoryAgentInput, params MemoryAgentParams) {
	if params.LLM == nil {
		return
	}

	log.Info("─── memory agent ───")

	// Capture the conversation snapshot NOW — before any subsequent turn can
	// write new messages to the DB. If we query lazily inside each tool call,
	// the next turn's messages may already be present and the classifier will
	// see the wrong context (it would reject "user built a grocery tool" because
	// the snippet shows "user asked about cortisol research" from the next turn).
	contextSnippet, _ := params.Store.RecentMessages(input.ConversationID, 2)

	// Load the memory agent prompt (hot-reloadable like other prompt files).
	promptContent := loadMemoryAgentPrompt(params.Cfg)

	// Build the turn transcript the model will review.
	transcript := buildMemoryTranscript(input, params.Store)

	// Pre-approved rewrites: shared between ClassifyWriteFunc (which populates it)
	// and the tool handlers (which check it). Same pattern as the main agent.
	preApproved := make(map[string]bool)

	// Build a minimal tools.Context — only the fields memory tools actually use.
	// No callbacks, no TUI bus, no scrub vault.
	// ClassifierSnippet carries the pre-captured context snapshot so
	// fact_helpers doesn't re-query the DB (which may have newer messages
	// from subsequent turns by the time classification runs).
	tctx := &tools.Context{
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

	// Tool definitions for the memory agent — the 4 memory tools plus done.
	// These are loaded from the same YAML registry as all other tools.
	memToolDefs := tools.LookupToolDefs(
		[]string{"save_memory", "save_self_memory", "update_memory", "remove_memory", "split_memory", "notify_agent", "done"},
		params.Cfg,
	)

	messages := []llm.ChatMessage{
		{Role: "system", Content: promptContent},
		{Role: "user", Content: transcript},
	}

	var totalCost float64

	// Memory agent loop limits — read from config with sensible defaults.
	// Same continuation window pattern as the main agent. If the memory
	// agent exhausts its iterations without calling done (e.g. during a
	// bulk cleanup), it gets a fresh window with a progress summary
	// injected.
	iterationsPerWindow := params.Cfg.MemoryAgent.IterationsPerWindow
	if iterationsPerWindow <= 0 {
		iterationsPerWindow = 15
	}
	maxContinuations := params.Cfg.MemoryAgent.MaxContinuations
	if maxContinuations <= 0 {
		maxContinuations = 2
	}

	// tracing tracks whether we have a live trace callback and accumulates
	// the formatted trace lines for the memory agent's slot.
	// The slot header (🧩 memory) is owned by the trace registry —
	// callers prepend it at render time — so we only send body
	// content here.
	tracing := params.TraceCallback != nil
	var traceLines []string

	emitMemTrace := func() {
		if !tracing || len(traceLines) == 0 {
			return
		}
		_ = params.TraceCallback(strings.Join(traceLines, "\n"))
	}

outer:
	for window := 0; window <= maxContinuations; window++ {
		if window > 0 {
			// Exhausted the previous window without a done signal.
			// Inject a continuation context so the model knows where it
			// left off. Reuses the same summary builder as the main agent.
			summary := buildContinuationSummary(traceLines)
			messages = append(messages, llm.ChatMessage{
				Role: "system",
				Content: fmt.Sprintf(
					"You have used all %d iterations in the previous window without calling done. "+
						"Continuation window %d of %d. Your progress so far:\n%s\n\n"+
						"Continue your work and call done (or notify_agent) when finished.",
					iterationsPerWindow, window, maxContinuations, summary,
				),
			})
			log.Infof("  [memory] continuation window %d/%d", window, maxContinuations)
			if tracing {
				traceLines = append(traceLines, fmt.Sprintf(
					"🔄 <i>continuation window %d/%d</i>", window, maxContinuations))
				emitMemTrace()
			}
		}

		for i := 0; i < iterationsPerWindow; i++ {
			resp, err := params.LLM.ChatCompletionWithTools(messages, memToolDefs)
			if err != nil {
				log.Error("memory agent: LLM error", "err", err)
				break outer
			}

			// Log cost and metrics — same as main agent.
			params.Store.SaveMetric(resp.Model, resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, 0, input.TriggerMsgID, resp.UsedFallback)
			totalCost += resp.CostUSD
			log.Infof("  [memory] tokens: %d prompt + %d completion | $%.6f | finish=%s",
				resp.PromptTokens, resp.CompletionTokens, resp.CostUSD, resp.FinishReason)

			if len(resp.ToolCalls) == 0 {
				// Model returned text or an empty response — stop the loop.
				break outer
			}

			// Append the assistant turn before executing tools.
			messages = append(messages, llm.ChatMessage{
				Role:      "assistant",
				Content:   resp.Content,
				ToolCalls: resp.ToolCalls,
			})

			// Execute each tool call, emit trace lines, and emit TUI events.
			for _, tc := range resp.ToolCalls {
				result := tools.Execute(tc.Function.Name, tc.Function.Arguments, tctx)
				isError := strings.HasPrefix(result, "error:") || strings.HasPrefix(result, "rejected:")
				log.Infof("    [memory] %s → %s", tc.Function.Name, truncateLog(result, 150))
				messages = append(messages, llm.ChatMessage{
					Role:       "tool",
					Content:    result,
					ToolCallID: tc.ID,
				})

				if params.Phase != nil {
					params.Phase.EmitToolCall(
						tc.Function.Name,
						truncateLog(tc.Function.Arguments, 200),
						truncateLog(result, 200),
						isError,
					)
				} else if params.EventBus != nil {
					// Fallback for callers that don't use the turn tracker yet.
					params.EventBus.Emit(tui.ToolCallEvent{
						Time:     time.Now(),
						TurnID:   input.TriggerMsgID,
						Source:   "memory",
						ToolName: tc.Function.Name,
						Args:     truncateLog(tc.Function.Arguments, 200),
						Result:   truncateLog(result, 200),
						IsError:  isError,
					})
				}

				if tracing {
					line := tools.FormatTrace(tc.Function.Name, tc.Function.Arguments, result)
					traceLines = append(traceLines, line)
					emitMemTrace()
				}
			}

			if tctx.DoneCalled {
				break outer
			}
		}

		// Inner loop exhausted without done. If at the hard cap, give up.
		if window == maxContinuations {
			log.Warn("[memory] hit max continuations without done signal",
				"total_calls", iterationsPerWindow*(window+1))
			if tracing {
				traceLines = append(traceLines, "⚠️ <i>max continuations reached</i>")
				emitMemTrace()
			}
			break outer
		}
	}

	log.Infof("  memory agent: %d memories saved | $%.6f", len(tctx.SavedMemories), totalCost)

	// Update the phase metrics — the caller's deferred phase.Done() will
	// merge these into the Tracker's accumulator. We update the deferred
	// Done's metrics by calling Done here with the real values. The
	// deferred Done in agent.go fires as a safety net with zero metrics
	// if we crash, but if we reach here, this call takes precedence
	// (PhaseHandle.Done is once-guarded).
	if params.Phase != nil {
		params.Phase.Done(turn.PhaseMetrics{
			Cost:          totalCost,
			MemoriesSaved: len(tctx.SavedMemories),
		})
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

	// Check the inbox for tasks delegated by the main agent (via send_task).
	// Consumed atomically — once read here, they won't appear again.
	inboxMsgs, err := store.ConsumeInbox("memory")
	if err == nil && len(inboxMsgs) > 0 {
		b.WriteString("## Inbox — tasks from the main agent\n")
		b.WriteString("The main agent has delegated these tasks to you. Handle them alongside your normal memory work.\n\n")
		for _, msg := range inboxMsgs {
			fmt.Fprintf(&b, "### Task: %s\n%s\n\n", msg.MsgType, msg.Payload)
		}
	}

	// Show existing memories for dedup context — the model should not re-save
	// anything already here. Cap at 30 to avoid flooding the context.
	memories, err := store.AllActiveMemories()
	if err == nil && len(memories) > 0 {
		b.WriteString("## Existing memories (do NOT re-save these)\n")
		limit := 30
		if len(memories) < limit {
			limit = len(memories)
		}
		for _, m := range memories[:limit] {
			fmt.Fprintf(&b, "- [ID=%d] %s\n", m.ID, m.Content)
		}
	}

	return b.String()
}
