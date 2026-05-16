// Package agent — post-turn introspection agent.
//
// After the memory and mood agents complete, RunIntrospectionAgent reviews
// the turn's think traces, reply, and existing self-knowledge to extract
// observations about Mira's own communication patterns and identity.
//
// Most turns it will call skip (nothing to reflect on). When it does find
// something, it saves self-memories via save_self_memory or updates existing
// ones via update_memory — the same tools the memory agent uses, but filtered
// to self-subject only via the SelfOnly context flag.
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
	"her/trace"
	"her/tui"
	"her/turn"

	_ "her/tools/done"
	_ "her/tools/list_cards"
	_ "her/tools/recall_memories"
	_ "her/tools/save_self_memory"
	_ "her/tools/skip"
	_ "her/tools/think"
	_ "her/tools/update_memory"
)

func init() {
	turn.Register(turn.Phase{Name: "introspection", Order: 400, Emoji: "🪡", Label: "introspection"})
	trace.Register(trace.Stream{Name: "introspection", Order: 400, Label: "🪡 <b>introspection</b>"})
}

// IntrospectionAgentInput is the turn data passed to the introspection agent.
// All fields are snapshotted AFTER memory + mood complete — the goroutine
// waits on a WaitGroup before reading self-memories and persona.md.
type IntrospectionAgentInput struct {
	UserMessage    string
	ThinkTraces    []string
	ReplyText      string
	TriggerMsgID   int64
	ConversationID string
	SelfMemories   []memory.Memory // snapshotted after memory+mood complete
	PersonaText    string          // snapshotted persona.md contents
}

// IntrospectionAgentParams bundles the dependencies the introspection agent needs.
type IntrospectionAgentParams struct {
	LLM           *llm.Client
	ClassifierLLM *llm.Client
	Store         memory.Store
	EmbedClient   *embed.Client
	Cfg           *config.Config
	TraceCallback tools.TraceCallback
	EventBus      *tui.Bus
	AgentEventCB  tools.AgentEventCallback
	Phase         *turn.PhaseHandle
}

// defaultIntrospectionAgentPrompt is used when introspection_agent_prompt.md can't be loaded.
const defaultIntrospectionAgentPrompt = `You are {{her}}'s self-reflection agent. After each conversation turn, review your think traces and reply to observe patterns about yourself.

Tools: think, list_cards, recall_memories, save_self_memory, update_memory, skip, done.

Rules:
- SKIP is the default. Most turns reveal nothing new about who you are.
- Identity over technique: "I'm drawn to cooking metaphors" = save. "I used a cooking metaphor" = skip.
- Build depth: if a pattern is already saved, only update if you have genuinely new insight.
- Every observation must be grounded in evidence from this turn's think traces or reply.
- Every save_self_memory MUST specify a card_slug (one of the my-* self cards).
- Call skip when there's nothing worth reflecting on. Call done after saving.`

// RunIntrospectionAgent reviews the turn for self-observations. Designed to
// run in a goroutine after memory + mood complete. Logs results but never
// returns errors — a missed self-observation is acceptable; a crash is not.
func RunIntrospectionAgent(input IntrospectionAgentInput, params IntrospectionAgentParams) {
	if params.LLM == nil {
		return
	}

	log.Info("─── introspection agent ───")

	// Snapshot the conversation context for classifier use (same pattern
	// as memory agent — capture before lazy queries can see future turns).
	contextSnippet, _ := params.Store.RecentMessages(input.ConversationID, 2)

	promptContent := loadIntrospectionAgentPrompt(params.Cfg)
	transcript := buildIntrospectionTranscript(input)

	preApproved := make(map[string]bool)

	// Build tools.Context with SelfOnly=true — recall_memories and list_cards
	// will only return self-subject data.
	tctx := &tools.Context{
		AgentName:           "introspection",
		Store:               params.Store,
		EmbedClient:         params.EmbedClient,
		SimilarityThreshold: params.Cfg.Embed.SimilarityThreshold,
		ClassifierLLM:       params.ClassifierLLM,
		Cfg:                 params.Cfg,
		ConversationID:      input.ConversationID,
		TriggerMsgID:        input.TriggerMsgID,
		PreApprovedRewrites: preApproved,
		ClassifierSnippet:   contextSnippet,
		SelfOnly:            true,
		EventBus:            params.EventBus,
		Phase:               params.Phase,
	}

	// Tool set for the introspection agent — driven by tool.yaml agent fields.
	introToolDefs := tools.ToolDefsForAgent("introspection", params.Cfg)

	messages := []llm.ChatMessage{
		{Role: "system", Content: promptContent},
		{Role: "user", Content: transcript},
	}

	var totalCost float64

	// Loop limits — smaller than other agents since most turns should skip.
	iterationsPerWindow := params.Cfg.IntrospectionAgent.IterationsPerWindow
	if iterationsPerWindow <= 0 {
		iterationsPerWindow = 5
	}
	if iterationsPerWindow > 15 {
		iterationsPerWindow = 15
	}
	maxContinuations := params.Cfg.IntrospectionAgent.MaxContinuations
	if maxContinuations <= 0 {
		maxContinuations = 1
	}
	if maxContinuations > 5 {
		maxContinuations = 5
	}

	tracing := params.TraceCallback != nil
	var traceLines []string

	emitTrace := func() {
		if !tracing || len(traceLines) == 0 {
			return
		}
		_ = params.TraceCallback(strings.Join(traceLines, "\n"))
	}

outer:
	for window := 0; window <= maxContinuations; window++ {
		if window > 0 {
			summary := buildContinuationSummary(traceLines)
			messages = append(messages, llm.ChatMessage{
				Role: "system",
				Content: fmt.Sprintf(
					"You have used all %d iterations in the previous window without finishing. "+
						"Continuation window %d of %d. Progress:\n%s\n\n"+
						"Call skip or done now.",
					iterationsPerWindow, window, maxContinuations, summary,
				),
			})
			log.Infof("  [introspection] continuation window %d/%d", window, maxContinuations)
		}

		for i := 0; i < iterationsPerWindow; i++ {
			start := time.Now()
			resp, err := params.LLM.ChatCompletionWithTools(messages, introToolDefs)
			latencyMs := int(time.Since(start).Milliseconds())

			if err != nil {
				log.Error("introspection agent: LLM error", "err", err)
				break outer
			}

			totalCost += resp.CostUSD
			log.Infof("  [introspection] tokens: %d prompt + %d completion | $%.6f | %dms",
				resp.PromptTokens, resp.CompletionTokens, resp.CostUSD, latencyMs)

			params.Store.SaveMetric(
				resp.Model,
				resp.PromptTokens,
				resp.CompletionTokens,
				resp.TotalTokens,
				resp.CostUSD,
				latencyMs,
				input.TriggerMsgID,
				resp.UsedFallback,
			)

			// No tool calls = model finished without calling done/skip.
			if len(resp.ToolCalls) == 0 {
				log.Warn("[introspection] no tool calls in response, finishing")
				break outer
			}

			// Execute each tool call and build trace/message history.
			for _, tc := range resp.ToolCalls {
				result := tools.Execute(tc.Function.Name, tc.Function.Arguments, tctx)

				// Trace line for observability.
				traceLine := fmt.Sprintf("    [introspection] %s → %s", tc.Function.Name, truncateLog(result, 80))
				log.Info(traceLine)
				traceLines = append(traceLines, fmt.Sprintf("%s → %s", tc.Function.Name, truncateLog(result, 60)))

				// Emit TUI event.
				if params.Phase != nil {
					params.Phase.EmitToolCall(tc.Function.Name, tc.Function.Arguments, truncateLog(result, 120), false)
				}

				// Append assistant + tool result to message history (for
				// the next LLM call in the loop).
				messages = append(messages,
					llm.ChatMessage{
						Role:      "assistant",
						Content:   "",
						ToolCalls: []llm.ToolCall{tc},
					},
					llm.ChatMessage{
						Role:       "tool",
						Content:    result,
						ToolCallID: tc.ID,
					},
				)
			}

			emitTrace()

			if tctx.DoneCalled {
				break outer
			}
		}

		if window == maxContinuations {
			log.Warn("[introspection] hit max continuations without done/skip signal")
			break outer
		}
	}

	memoriesSaved := len(tctx.SavedMemories)
	log.Infof("  introspection agent: %d self-memories saved | $%.6f", memoriesSaved, totalCost)

	// Final trace update.
	emitTrace()
}

// loadIntrospectionAgentPrompt reads the hot-reloadable prompt file.
// Falls back to the embedded default if the file isn't found.
func loadIntrospectionAgentPrompt(cfg *config.Config) string {
	// Look for the prompt file next to the main agent prompt.
	promptPath := "introspection_agent_prompt.md"
	if cfg.Persona.AgentPromptFile != "" {
		dir := filepath.Dir(cfg.Persona.AgentPromptFile)
		promptPath = filepath.Join(dir, "introspection_agent_prompt.md")
	}

	data, err := os.ReadFile(promptPath)
	if err != nil {
		log.Warn("introspection agent: prompt file not found, using fallback", "path", promptPath)
		return cfg.ExpandPrompt(defaultIntrospectionAgentPrompt)
	}
	return cfg.ExpandPrompt(string(data))
}

// buildIntrospectionTranscript assembles the 5-section context the
// introspection agent sees — what happened, how it happened, and what
// it already knows about itself.
func buildIntrospectionTranscript(input IntrospectionAgentInput) string {
	var b strings.Builder

	// Section 1: What the user said
	b.WriteString("## What the user said\n\n")
	b.WriteString(input.UserMessage)
	b.WriteString("\n\n")

	// Section 2: What I said
	b.WriteString("## What I said\n\n")
	b.WriteString(input.ReplyText)
	b.WriteString("\n\n")

	// Section 3: How I arrived at this reply
	b.WriteString("## How I arrived at this reply\n\n")
	if len(input.ThinkTraces) > 0 {
		for _, t := range input.ThinkTraces {
			b.WriteString("- ")
			b.WriteString(t)
			b.WriteString("\n")
		}
	} else {
		b.WriteString("No thinking traces for this turn.\n")
	}
	b.WriteString("\n")

	// Section 4: What I already know about myself
	b.WriteString("## What I already know about myself\n\n")
	if len(input.SelfMemories) > 0 {
		for _, m := range input.SelfMemories {
			fmt.Fprintf(&b, "- [ID=%d] %s\n", m.ID, m.Content)
		}
	} else {
		b.WriteString("No self-memories yet.\n")
	}
	b.WriteString("\n")

	// Section 5: My current self-image
	b.WriteString("## My current self-image\n\n")
	if input.PersonaText != "" {
		b.WriteString(input.PersonaText)
	} else {
		b.WriteString("No persona file configured.\n")
	}

	return b.String()
}
