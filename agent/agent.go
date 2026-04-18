package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"her/layers"
	"her/compact"
	"her/config"
	"her/embed"
	"her/llm"
	"her/logger"
	"her/memory"
	"her/scrub"
	"her/search"
	"her/tools"
	"her/tui"

	// Blank imports trigger init() registration for each tool's handler.
	// Same pattern as database drivers: import _ "github.com/lib/pq"
	// Each import causes the package's init() to run, which calls
	// tools.Register("name", Handle) to add the handler to the registry.
	_ "her/tools/done"
	_ "her/tools/recall_memories"
	_ "her/tools/reply"
	_ "her/tools/think"
	_ "her/tools/update_persona"
	_ "her/tools/use_tools"
	_ "her/tools/view_image"
	_ "her/tools/web_read"
	_ "her/tools/web_search"
)

// log is the package-level logger for the agent package.
var log = logger.WithPrefix("agent")

// Callback types and toolContext have been moved to the tools package
// (tools/context.go). The agent imports them as tools.Context,
// tools.StatusCallback, etc. See tools/context.go for documentation.

// defaultAgentPrompt is used as a fallback if main_agent_prompt.md can't be loaded.
// Uses {{her}} placeholder so it still works with the template expansion.
const defaultAgentPrompt = `You are {{her}}'s brain. You orchestrate every response. Call think to reason, reply to respond, memory tools to remember, and done when finished. Every turn must include reply and done.`

// loadAgentPrompt reads the agent prompt from disk (hot-reloadable),
// falling back to a minimal default if the file doesn't exist.
// After reading, it replaces tool inventory markers with auto-generated
// sections from the YAML registry, then expands {{her}}/{{user}} placeholders.
// This is the same pattern as prompt.md — edit the file, restart the
// bot (or it reloads on next message), and the behavior changes.
func loadAgentPrompt(path string, cfg *config.Config) string {
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		log.Warn("couldn't load agent prompt, using default", "path", path)
		return cfg.ExpandPrompt(defaultAgentPrompt)
	}
	content := expandToolSections(string(data))
	return cfg.ExpandPrompt(content)
}

// expandToolSections replaces content between marker comments in the
// agent prompt with auto-generated tool inventory from the YAML registry.
// Markers look like <!-- BEGIN HOT_TOOLS --> ... <!-- END HOT_TOOLS -->.
// If markers are missing, the content is returned unchanged.
func expandToolSections(content string) string {
	content = replaceBetweenMarkers(content, "HOT_TOOLS", tools.RenderHotToolsList())
	content = replaceBetweenMarkers(content, "CATEGORY_TABLE", tools.RenderCategoryTable())
	return content
}

// replaceBetweenMarkers replaces the text between <!-- BEGIN tag --> and
// <!-- END tag --> with the replacement string. The markers themselves
// are preserved; only the content between them is swapped.
func replaceBetweenMarkers(content, tag, replacement string) string {
	begin := "<!-- BEGIN " + tag + " -->"
	end := "<!-- END " + tag + " -->"
	startIdx := strings.Index(content, begin)
	endIdx := strings.Index(content, end)
	if startIdx < 0 || endIdx < 0 || endIdx <= startIdx {
		return content // markers not found, return unchanged
	}
	return content[:startIdx+len(begin)] + "\n" + replacement + "\n" + content[endIdx:]
}

// RunParams bundles all the parameters for an agent run.
// This replaces the old 12+ argument function signature with a single
// struct, making it much easier to add new parameters without breaking
// every caller.
//
// In Python you might use **kwargs or a dataclass. In Go, a params struct
// is the idiomatic way to handle functions with many inputs.
type RunParams struct {
	AgentLLM                  *llm.Client
	MemoryAgentLLM            *llm.Client // post-turn background memory agent — nil if not configured
	ChatLLM                   *llm.Client
	VisionLLM                 *llm.Client // vision language model — nil if not configured
	ClassifierLLM             *llm.Client // classifier for memory writes — nil if not configured
	Store                     *memory.Store
	EmbedClient               *embed.Client
	SimilarityThreshold       float64
	TavilyClient              *search.TavilyClient
	Cfg                       *config.Config
	ScrubbedUserMessage       string
	ScrubVault                *scrub.Vault
	ConversationID            string
	TriggerMsgID              int64
	StatusCallback            tools.StatusCallback
	SendCallback              tools.SendCallback
	TTSCallback               tools.TTSCallback
	TraceCallback             tools.TraceCallback             // nil if traces disabled
	MemoryTraceCallback       tools.TraceCallback             // nil if memory agent tracing disabled; separate message from TraceCallback
	StageResetCallback        tools.StageResetCallback        // nil-safe — sends new placeholder after reply
	DeletePlaceholderCallback tools.DeletePlaceholderCallback // nil-safe — deletes orphan placeholder on exit
	SendConfirmCallback       tools.SendConfirmCallback       // nil-safe — confirmation buttons for destructive actions
	StreamCallback            tools.StreamCallback            // nil-safe — streams chat tokens to Telegram for live typing effect
	ImageBase64               string   // base64-encoded image data (empty if no image)
	ImageMIME                 string   // MIME type of the image (e.g., "image/jpeg")
	OCRText                   string   // pre-flight OCR text extracted from the photo (empty if no image or OCR unavailable)
	EventBus                  *tui.Bus // nil-safe — emits rich typed events for the TUI
	ConfigPath                string   // path to config.yaml — needed for persisting location changes via set_location
}

// RunResult holds the outcome of an agent run — the reply text plus
// metrics that the bot needs for the TUI. Adding fields here is cheap
// (it's just a struct return), and avoids the bot having to query the
// DB or re-derive data the agent already has in memory.
type RunResult struct {
	ReplyText  string
	TotalCost  float64 // accumulated cost across all LLM calls (agent + chat)
	ToolCalls  int     // number of tool calls the agent made
	FactsSaved int     // number of facts saved/updated during this turn
}

// Run executes the agent loop for one conversation turn.
// This is the core orchestration — the agent decides what tools to call
// (search, read, book lookup, memory ops) and MUST call reply exactly once
// to generate the user-facing response.
//
// Unlike the old architecture where this ran in a background goroutine,
// Run now executes SYNCHRONOUSLY because it IS the response pipeline.
// The persona evolution triggers at the end still run in a goroutine
// since they don't affect the user's response.
func Run(params RunParams) (*RunResult, error) {
	log.Info("─── agent ───")

	// Helper for nil-safe event emission — avoids if-checks everywhere.
	emit := func(e tui.Event) {
		if params.EventBus != nil {
			params.EventBus.Emit(e)
		}
	}

	// --- Compaction ---
	// Load a wider window (40 messages) so MaybeCompact can actually
	// see enough history to trigger. Previously we fed it only the
	// recent_messages window (10), which was always under the token
	// threshold — so compaction never fired and older messages just
	// vanished from context with no summary.
	const compactionWindow = 100
	compactionMsgs, err := params.Store.RecentMessages(params.ConversationID, compactionWindow)
	if err != nil {
		log.Error("loading compaction history", "err", err)
	}
	// Strip the current message to avoid duplication (it's shown
	// separately under "Current Message" in the agent context).
	if params.TriggerMsgID > 0 && len(compactionMsgs) > 0 {
		filtered := make([]memory.Message, 0, len(compactionMsgs))
		for _, msg := range compactionMsgs {
			if msg.ID != params.TriggerMsgID {
				filtered = append(filtered, msg)
			}
		}
		compactionMsgs = filtered
	}

	// Run compaction on the wider window. MaybeCompact summarizes
	// older messages and returns only the messages that should stay
	// in full fidelity. The summary gets injected into the system
	// prompt by buildChatSystemPrompt.
	var conversationSummary string
	var keptMessages []memory.Message
	if len(compactionMsgs) > 0 {
		emit(tui.CompactStartEvent{Time: time.Now(), Stream: "chat"})
		cr, compactErr := compact.MaybeCompact(
			params.ChatLLM, params.Store, params.ConversationID,
			compactionMsgs, params.Cfg.Memory.MaxHistoryTokens,
			params.Cfg.Identity.Her, params.Cfg.Identity.User,
		)
		if compactErr != nil {
			log.Error("compaction error", "err", compactErr)
			keptMessages = compactionMsgs
		} else {
			conversationSummary = cr.Summary
			keptMessages = cr.KeptMessages

			// Surface compaction in traces and TUI so it's not invisible.
			if cr.DidCompact {
				if params.TraceCallback != nil {
					params.TraceCallback(fmt.Sprintf(
						"📦 <i>compacted %d messages (%d→%d tokens)</i>",
						cr.Summarized, cr.TokensBefore, cr.TokensAfter))
				}
				emit(tui.CompactEvent{
					Time:         time.Now(),
					Summarized:   cr.Summarized,
					TokensBefore: cr.TokensBefore,
					TokensAfter:  cr.TokensAfter,
				})
			}
		}
	} else {
		keptMessages = compactionMsgs
	}

	// Trim to the configured sliding window for the agent context.
	// The agent only needs the last N messages for resolving references
	// like "it", "that book", etc. — the summary covers older context.
	recentMsgs := keptMessages
	if len(recentMsgs) > params.Cfg.Memory.RecentMessages {
		recentMsgs = recentMsgs[len(recentMsgs)-params.Cfg.Memory.RecentMessages:]
	}

	// --- Agent action history compaction ---
	// Load the agent's tool call history from agent_turns and compact it
	// if it's getting too large. This gives the agent persistent memory
	// of what it did in previous turns (facts saved, searches run, etc.).
	var agentActionSummary string
	var recentAgentActions []memory.AgentAction
	agentActions, err := params.Store.RecentAgentActions(params.ConversationID, 30) // last 30 messages worth
	if err != nil {
		log.Warn("failed to load agent actions", "err", err)
	} else if len(agentActions) > 0 {
		emit(tui.CompactStartEvent{Time: time.Now(), Stream: "agent"})
		acr, compactErr := compact.MaybeCompactAgent(
			params.ChatLLM, params.Store, params.ConversationID,
			agentActions, params.Cfg.Memory.AgentContextBudget,
			params.Cfg.Identity.Her,
		)
		if compactErr != nil {
			log.Error("agent compaction error", "err", compactErr)
			recentAgentActions = agentActions
		} else {
			agentActionSummary = acr.Summary
			recentAgentActions = acr.RecentActions
			if acr.DidCompact {
				log.Infof("  agent compacted %d actions (%d→%d tokens)",
					acr.Summarized, acr.TokensBefore, acr.TokensAfter)
			}
		}
	}

	// Semantic search — find facts most relevant to what the user just said.
	// This is the core of v0.4: instead of showing the LLM ALL facts sorted
	// by importance, we embed the user's message and find the closest matches
	// via sqlite-vec KNN. The results go into the system prompt so the
	// conversational model has the right context without seeing everything.
	//
	// Query context: we prepend up to 2 prior user messages so the embedding
	// captures conversational intent, not just the latest message. Without
	// this, "vet says it might be his kidneys" embeds as health/medical —
	// with "my dog max has been sick" prepended, it correctly pulls pet facts too.
	var relevantFacts []memory.Fact
	if params.EmbedClient != nil && params.Store.EmbedDimension > 0 {
		queryText := params.ScrubbedUserMessage
		if len(recentMsgs) > 0 {
			var priorUserMsgs []string
			for i := len(recentMsgs) - 1; i >= 0 && len(priorUserMsgs) < 2; i-- {
				if recentMsgs[i].Role == "user" {
					content := recentMsgs[i].ContentScrubbed
					if content == "" {
						content = recentMsgs[i].ContentRaw
					}
					priorUserMsgs = append([]string{content}, priorUserMsgs...)
				}
			}
			if len(priorUserMsgs) > 0 {
				queryText = strings.Join(priorUserMsgs, " | ") + " | " + params.ScrubbedUserMessage
			}
		}
		queryVec, err := params.EmbedClient.Embed(queryText)
		if err != nil {
			log.Warn("semantic search: embedding failed, falling back to importance-only", "err", err)
		} else {
			relevantFacts, err = params.Store.SemanticSearch(queryVec, params.Cfg.Memory.MaxFactsInContext)
			if err != nil {
				log.Warn("semantic search: query failed, falling back to importance-only", "err", err)
			} else {
				log.Infof("  semantic search: %d relevant facts", len(relevantFacts))
			}
		}
	}

	// Emit context event for the TUI
	emit(tui.ContextEvent{
		Time: time.Now(), TurnID: params.TriggerMsgID,
		RelevantFacts: len(relevantFacts),
	})

	// Build the context message for the agent using the layer registry.
	// Each layer (time, history, message, image, facts) lives in its own
	// file under agent/layers/ and registers itself via init().
	layerCtx := &layers.LayerContext{
		Store:               params.Store,
		Cfg:                 params.Cfg,
		EmbedClient:         params.EmbedClient,
		RelevantFacts:       relevantFacts,
		ConversationSummary: conversationSummary,
		AgentActionSummary:  agentActionSummary,
		RecentAgentActions:  recentAgentActions,
		ConversationID:      params.ConversationID,
		ScrubbedUserMessage: params.ScrubbedUserMessage,
		RecentMessages:      recentMsgs,
		HasImage:            params.ImageBase64 != "",
		OCRText:             params.OCRText,
	}
	context, agentLayerResults := layers.BuildAll(layers.StreamAgent, layerCtx)

	// Log the agent context shape for observability.
	var agentTotalTokens int
	for _, lr := range agentLayerResults {
		agentTotalTokens += lr.Tokens
		if lr.Detail != "" {
			log.Infof("  [agent layer] %s: ~%d tokens (%s)", lr.Name, lr.Tokens, lr.Detail)
		} else {
			log.Infof("  [agent layer] %s: ~%d tokens", lr.Name, lr.Tokens)
		}
	}
	log.Infof("  agent context total: ~%d tokens", agentTotalTokens)

	// Load the agent prompt from disk (hot-reloadable, like prompt.md).
	agentPrompt := loadAgentPrompt(params.Cfg.Persona.AgentPromptFile, params.Cfg)

	// Set up the conversation with the agent model.
	messages := []llm.ChatMessage{
		{Role: "system", Content: agentPrompt},
		{Role: "user", Content: context},
	}

	// Start with only the hot tools (7 instead of 26). The agent can
	// load deferred tools on demand via use_tools(["search"]) etc.
	// This reduces context pressure on the agent model significantly.
	toolDefs := tools.HotToolDefs(params.Cfg)

	// Build the tool context with everything the tools need.
	tctx := &tools.Context{
		Store:                     params.Store,
		EmbedClient:               params.EmbedClient,
		SimilarityThreshold:       params.SimilarityThreshold,
		PersonaFile:               params.Cfg.Persona.PersonaFile,
		StatusCallback:            params.StatusCallback,
		SendCallback:              params.SendCallback,
		TTSCallback:               params.TTSCallback,
		TraceCallback:             params.TraceCallback,
		StageResetCallback:        params.StageResetCallback,
		DeletePlaceholderCallback: params.DeletePlaceholderCallback,
		SendConfirmCallback:       params.SendConfirmCallback,
		StreamCallback:            params.StreamCallback,
		ChatLLM:                   params.ChatLLM,
		VisionLLM:                 params.VisionLLM,
		ClassifierLLM:             params.ClassifierLLM,
		TavilyClient:              params.TavilyClient,
		Cfg:                       params.Cfg,
		ScrubVault:                params.ScrubVault,
		ScrubbedUserMessage:       params.ScrubbedUserMessage,
		ConversationID:            params.ConversationID,
		TriggerMsgID:              params.TriggerMsgID,
		ConversationSummary:       conversationSummary,
		RelevantFacts:             relevantFacts,
		ImageBase64:               params.ImageBase64,
		ImageMIME:                 params.ImageMIME,
		OCRText:                   params.OCRText,
		ActiveTools:               &toolDefs,
		EventBus:                  params.EventBus,
		ConfigPath:                params.ConfigPath,
		PreApprovedRewrites:       make(map[string]bool),
	}

	// Tool-calling loop. The model may return multiple tool calls,
	// or it may return tool calls that require a follow-up turn.
	// Track turn index for agent_turns logging.
	turnIndex := 0

	// We loop up to 10 iterations to allow for think + search + refine cycles.
	// With the think tool, a typical complex flow might use 6-7 iterations:
	// think → search → think(evaluate) → search(refine) → think → reply → save_fact
	// --- Agent tool-calling loop ---
	// Modeled after Crush (charmbracelet/fantasy): loop while the model
	// keeps returning tool calls (finish_reason == "tool_calls"). When the
	// model stops calling tools (finish_reason == "stop"), the loop ends.
	// First iteration uses tool_choice="required" to nudge the model
	// into the tool-calling flow. After that, "auto" lets it drive.
	// The fallback handler still exists for resilience.
	//
	// Loop detection: track think content to catch the agent repeating
	// itself. Crush uses SHA-256 signatures; we keep it simpler since
	// our tool set is smaller and think loops are the main failure mode.
	var lastThinkContent string
	var repeatCount int
	// agentFinalText captures any text the agent outputs when it stops
	// calling tools. Used as fallback instruction if reply wasn't called.
	var agentFinalText string

	// --- Metrics accumulators ---
	// Track cost and tool calls across the entire agent run so we can
	// return them in RunResult for the TUI's TurnEndEvent.
	var totalCost float64
	var totalToolCalls int

	// --- Think trace collector ---
	// Captures the raw content of every think() call for the memory agent.
	// Separate from traceLines (which is formatted HTML for Telegram) —
	// the memory agent needs the raw thought text, not the Telegram markup.
	var thinkTraces []string

	// --- Trace builder ---
	// Accumulates formatted trace lines as the agent executes. If tracing
	// is enabled, the trace message gets sent/updated after each tool call
	// so the user can watch the agent think in real time.
	var traceLines []string
	tracing := tctx.TraceCallback != nil

	// sendTrace pushes the current trace to Telegram (sends or edits).
	sendTrace := func() {
		if !tracing || len(traceLines) == 0 {
			return
		}
		text := strings.Join(traceLines, "\n")
		if err := tctx.TraceCallback(text); err != nil {
			log.Warn("trace: failed to send/update", "err", err)
		}
	}

	// Agent loop constants. The outer loop provides continuation windows —
	// if the agent runs out of iterations without calling done, it gets
	// a fresh window (up to maxContinuations times) with a summary of
	// progress so far injected as context.
	const (
		iterationsPerWindow = 15
		maxContinuations    = 3 // 4 windows total = 60 calls hard cap
	)

outer:
	for window := 0; window <= maxContinuations; window++ {
		if window > 0 {
			// We exhausted the previous window without a done signal.
			// Inject a continuation context so the agent knows where it
			// left off and is prompted to update the user immediately.
			summary := buildContinuationSummary(traceLines)
			messages = append(messages, llm.ChatMessage{
				Role: "system",
				Content: fmt.Sprintf(
					"You have used all %d iterations in the previous window without calling done. "+
						"Continuation window %d of %d. Your progress so far:\n%s\n\n"+
						"IMPORTANT: Call reply immediately to update the user on your progress, "+
						"then continue your work and call done when finished.",
					iterationsPerWindow, window, maxContinuations, summary,
				),
			})
			log.Infof("  continuation window %d/%d", window, maxContinuations)
			if tracing {
				traceLines = append(traceLines, fmt.Sprintf(
					"🔄 <i>continuation window %d/%d</i>", window, maxContinuations))
				sendTrace()
			}
		}

		for i := 0; i < iterationsPerWindow; i++ {
			// Nudge on the first iteration only: tool_choice="required"
			// forces the model into the tool-calling flow. Without this,
			// Trinity occasionally skips tools entirely and outputs plain
			// text on iter 0. After the first call, "auto" lets the model
			// drive naturally (it exits via the done tool).
			var toolChoice interface{}
			if i == 0 && window == 0 {
				toolChoice = "required"
			}
			resp, err := params.AgentLLM.ChatCompletionWithTools(messages, toolDefs, toolChoice)
			if err != nil {
				// The LLM client handles fallback automatically on retriable
				// errors (429, 500-503, timeout). If we still get an error here,
				// both primary and fallback failed — bail out of the agent loop.
				log.Error("LLM error (primary + fallback both failed)", "err", err)
				if tracing {
					traceLines = append(traceLines, fmt.Sprintf("❌ <b>error:</b> %s", truncateLog(err.Error(), 100)))
					sendTrace()
				}
				break outer
			}

			// Log agent metrics and accumulate cost for RunResult.
			params.Store.SaveMetric(resp.Model, resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, 0, params.TriggerMsgID)
			totalCost += resp.CostUSD
			log.Infof("  tokens: %d prompt + %d completion | $%.6f | finish=%s",
				resp.PromptTokens, resp.CompletionTokens, resp.CostUSD, resp.FinishReason)

			// Surface model fallback in traces so it's visible in Telegram.
			if tracing && resp.UsedFallback {
				traceLines = append(traceLines, fmt.Sprintf("⚡ <i>agent fallback: %s</i>", resp.Model))
				sendTrace()
			}
			emit(tui.AgentIterEvent{
				Time: time.Now(), TurnID: params.TriggerMsgID, Iteration: i + window*iterationsPerWindow,
				PromptTokens: resp.PromptTokens, CompletionTokens: resp.CompletionTokens,
				CostUSD: resp.CostUSD, FinishReason: resp.FinishReason,
			})

			// --- Check finish_reason to decide how to proceed ---
			hasToolCalls := len(resp.ToolCalls) > 0

			if !hasToolCalls {
				if resp.Content != "" {
					trimmed := strings.TrimSpace(strings.ToLower(resp.Content))
					// If the agent just typed "done" as text instead of calling
					// the done tool, treat it as a done signal. Some models do this.
					if trimmed == "done" || trimmed == "done." {
						log.Info("  agent typed 'done' as text (treating as done signal)")
						break outer
					}
					// Model returned plain text instead of a tool call. Kimi K2.5
					// and Trinity are thinking models — if they skip tool calls it's
					// a prompting problem, not a model capability problem. Save the
					// text as a fallback instruction and exit gracefully.
					log.Warnf("  agent returned text instead of tool calls: %s", truncateLog(resp.Content, 200))
					agentFinalText = resp.Content
					break outer
				}
				log.Info("  done (no actions)")
				break outer
			}

			// --- Loop detection ---
			if len(resp.ToolCalls) == 1 && resp.ToolCalls[0].Function.Name == "think" {
				if resp.ToolCalls[0].Function.Arguments == lastThinkContent {
					repeatCount++
					if repeatCount >= 2 {
						log.Warn("think loop detected, forcing exit", "repeats", repeatCount+1)
						if tracing {
							traceLines = append(traceLines, "⚠️ <i>loop detected — forcing exit</i>")
							sendTrace()
						}
						break outer
					}
				} else {
					lastThinkContent = resp.ToolCalls[0].Function.Arguments
					repeatCount = 0
				}
			} else {
				lastThinkContent = ""
				repeatCount = 0
			}

			log.Infof("  %d tool call(s):", len(resp.ToolCalls))

			// Append the assistant message with tool calls to the conversation.
			messages = append(messages, llm.ChatMessage{
				Role:      "assistant",
				Content:   resp.Content,
				ToolCalls: resp.ToolCalls,
			})

			// Execute each tool call, feed results back to the model,
			// and build trace lines for observability.
			for _, tc := range resp.ToolCalls {
				totalToolCalls++
				params.Store.SaveAgentTurn(params.TriggerMsgID, turnIndex, "assistant", tc.Function.Name, tc.Function.Arguments, "")
				turnIndex++

				// Capture think() content for the memory agent's transcript.
				// The memory agent uses raw thought text (not the Telegram-formatted
				// trace lines) to understand the agent's reasoning this turn.
				if tc.Function.Name == "think" {
					var thinkArgs struct {
						Thought string `json:"thought"`
					}
					if err := json.Unmarshal([]byte(tc.Function.Arguments), &thinkArgs); err == nil && thinkArgs.Thought != "" {
						thinkTraces = append(thinkTraces, thinkArgs.Thought)
					}
				}

				result := executeTool(tc, tctx)
				isError := strings.HasPrefix(result, "error:")
				log.Infof("    → %s: %s", tc.Function.Name, truncateLog(result, 200))
				emit(tui.ToolCallEvent{
					Time:     time.Now(),
					TurnID:   params.TriggerMsgID,
					ToolName: tc.Function.Name,
					Args:     truncateLog(tc.Function.Arguments, 200),
					Result:   truncateLog(result, 200),
					IsError:  isError,
				})

				// Build the trace line for this tool call.
				if tracing {
					line := formatTraceLine(tc.Function.Name, tc.Function.Arguments, result)
					// If the chat model fell back during a reply, annotate the trace
					// so it's obvious which model generated the user-facing response.
					if tc.Function.Name == "reply" && tctx.ReplyUsedFallback {
						var rArgs struct {
							Instruction string `json:"instruction"`
						}
						json.Unmarshal([]byte(tc.Function.Arguments), &rArgs)
						line = fmt.Sprintf("⚡ <b>reply(fallback → %s):</b> <i>%s</i>",
							tctx.ReplyModel, escapeHTML(truncateLog(rArgs.Instruction, 200)))
					}
					traceLines = append(traceLines, line)
					sendTrace()
				}

				params.Store.SaveAgentTurn(params.TriggerMsgID, turnIndex, "tool", tc.Function.Name, "", result)
				turnIndex++

				messages = append(messages, llm.ChatMessage{
					Role:       "tool",
					Content:    result,
					ToolCallID: tc.ID,
				})
			}

			// Exit when the agent explicitly signals it's done.
			// (The "done" trace line is already added by formatTraceLine above.)
			if tctx.DoneCalled {
				log.Info("  done signal received")
				break outer
			}

			// Also exit if finish_reason was "stop" even though tools were
			// present — some providers do this (the OpenCode #14972 bug).
			if resp.FinishReason == "stop" {
				log.Info("  finish_reason=stop after tool execution")
				break outer
			}
		}

		// Inner loop exhausted without a done signal. If we're at the hard
		// cap, give up. Otherwise the outer loop increments and injects
		// a continuation context for the next window.
		if window == maxContinuations {
			log.Warn("hit max continuations without done signal",
				"total_calls", iterationsPerWindow*(window+1))
			if tracing {
				traceLines = append(traceLines, "⚠️ <i>max continuations reached</i>")
				sendTrace()
			}
			break outer
		}
	}

	// --- Auto-done for diffusion models ---
	// Diffusion LLMs (like Mercury 2) generate output in parallel and
	// sometimes omit the done tool call even after completing all work.
	// If reply was called and the loop ended naturally (not via done),
	// treat it as a clean completion rather than an error.
	if tctx.ReplyCount > 0 && !tctx.DoneCalled {
		log.Info("auto-done: reply was called but done was not — treating as complete")
		tctx.DoneCalled = true
	}

	// --- Fallback: ensure the user always gets a response ---
	// If the agent never called the reply tool, we still need to respond.
	// We use replyCount (not replyCalled) because replyCalled gets reset
	// after each stage reset — but replyCount tracks lifetime replies.
	if tctx.ReplyCount == 0 {
		if tracing {
			traceLines = append(traceLines, "⚠️ <i>agent never called reply — using fallback</i>")
			sendTrace()
		}
		if agentFinalText != "" {
			log.Warn("reply was never called, using agent text as instruction")
			instruction := fmt.Sprintf(`{"instruction":%s}`, mustJSON(agentFinalText))
			fallbackResult := tools.Execute("reply", instruction, tctx)
			if tctx.ReplyCount == 0 {
				log.Error("fallback reply failed", "result", fallbackResult)
				return nil, fmt.Errorf("agent failed to generate a reply")
			}
		} else {
			log.Warn("reply was never called, generating generic fallback")
			fallbackResult := tools.Execute("reply", `{"instruction":"The user sent a message. Respond naturally. Do not reference any interruption or claim you were cut off."}`, tctx)
			if tctx.ReplyCount == 0 {
				log.Error("fallback reply also failed", "result", fallbackResult)
				return nil, fmt.Errorf("agent failed to generate a reply")
			}
		}
	}

	// --- Cleanup: delete orphan placeholder ---
	// The last stage reset (after the final reply) sends a new 💭
	// placeholder that never gets used. If replyCalled is false but
	// we DID reply at least once, the current placeholder is orphaned.
	if !tctx.ReplyCalled && tctx.ReplyCount > 0 && tctx.DeletePlaceholderCallback != nil {
		if err := tctx.DeletePlaceholderCallback(); err != nil {
			log.Warn("cleanup: failed to delete orphan placeholder", "err", err)
		}
	}

	result := &RunResult{
		ReplyText:  tctx.ReplyText,
		TotalCost:  totalCost + tctx.ReplyCost,
		ToolCalls:  totalToolCalls,
		FactsSaved: len(tctx.SavedFacts),
	}

	// --- Persona Evolution Triggers + Memory Agent ---
	// These run AFTER the response has been sent to the user.
	// They go in a goroutine because they don't affect the current turn.
	//
	// The chain: facts accumulate → triggers reflection →
	//            reflections accumulate → triggers persona rewrite
	// No concept of "conversations" needed — just fact and reflection counts.
	go func() {
		// --- Memory agent ---
		// Runs first — we want facts saved before the reflection trigger
		// checks the fact count. This way a fact-rich turn can trigger
		// reflection in the same goroutine run.
		if params.MemoryAgentLLM != nil {
			RunMemoryAgent(
				MemoryAgentInput{
					UserMessage:    params.ScrubbedUserMessage,
					ThinkTraces:    thinkTraces,
					ReplyText:      result.ReplyText,
					TriggerMsgID:   params.TriggerMsgID,
					ConversationID: params.ConversationID,
				},
				MemoryAgentParams{
					LLM:           params.MemoryAgentLLM,
					ClassifierLLM: params.ClassifierLLM,
					Store:         params.Store,
					EmbedClient:   params.EmbedClient,
					Cfg:           params.Cfg,
					TraceCallback: params.MemoryTraceCallback,
					EventBus:      params.EventBus,
				},
			)
		}

	}()

	return result, nil
}

// executeTool runs a single tool call and returns a result string.
// If the tool call has truncated/malformed JSON arguments (usually from
// hitting max_tokens mid-generation), we return an error message that
// tells the model what happened so it can retry with shorter arguments.
func executeTool(tc llm.ToolCall, tctx *tools.Context) string {
	// Validate JSON before dispatching. Truncated tool calls happen when
	// the model hits max_tokens while generating the arguments JSON.
	// Rather than letting each tool fail with a confusing parse error,
	// give the model clear feedback so it can self-correct.
	if tc.Function.Arguments != "" && !json.Valid([]byte(tc.Function.Arguments)) {
		return fmt.Sprintf("error: malformed JSON in arguments (likely truncated by token limit). Please retry with shorter arguments. Got: %s", truncateLog(tc.Function.Arguments, 100))
	}

	switch tc.Function.Name {
	default:
		// Guard: only dispatch tools that are in the current active tool set.
		//
		// Because memory_agent.go imports save_fact/save_self_fact/etc. in the
		// same package, those handlers are registered in the global tools.Execute
		// registry. Without this check, the main agent (Trinity) can call them by
		// hallucinating tool calls for tools not in its schema — the handlers exist
		// so Execute succeeds, but the action is wrong (memory writes belong to Kimi).
		//
		// ActiveTools is the authoritative list of what's available this turn.
		// It starts as the hot tools and grows when use_tools loads a category.
		for _, t := range *tctx.ActiveTools {
			if t.Function.Name == tc.Function.Name {
				return tools.Execute(tc.Function.Name, tc.Function.Arguments, tctx)
			}
		}
		return fmt.Sprintf("error: tool '%s' is not available. Use use_tools to load additional categories if needed.", tc.Function.Name)
	}
}


// truncateLog shortens a string for log output, adding "..." if it was cut.
// mustJSON marshals a string to a JSON string literal (with quotes and
// escaping). Used to safely embed agent text into a JSON object without
// risking broken JSON from quotes or newlines in the content.
func mustJSON(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func truncateLog(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}

// buildContinuationSummary converts trace lines into a plain-text summary
// for injection into the continuation window context. Strips the HTML tags
// used for Telegram formatting so the model sees clean readable text.
// Capped at ~500 chars so it doesn't consume much of the agent's context.
func buildContinuationSummary(traceLines []string) string {
	// Strip HTML tags used for Telegram (b, i, and their closing forms).
	htmlReplacer := strings.NewReplacer(
		"<b>", "", "</b>", "",
		"<i>", "", "</i>", "",
		"&amp;", "&", "&lt;", "<", "&gt;", ">",
	)

	var parts []string
	for _, line := range traceLines {
		clean := htmlReplacer.Replace(line)
		clean = strings.TrimSpace(clean)
		if clean != "" {
			parts = append(parts, clean)
		}
	}

	summary := strings.Join(parts, "\n")
	const maxSummaryLen = 500
	if len(summary) > maxSummaryLen {
		summary = summary[:maxSummaryLen] + "..."
	}
	return summary
}

// formatTraceLine builds an HTML-formatted trace line for a single tool call.
// Delegates to tools.FormatTrace which uses YAML-defined trace specs.
func formatTraceLine(toolName, argsJSON, result string) string {
	return tools.FormatTrace(toolName, argsJSON, result)
}

// escapeHTML escapes special characters for Telegram's HTML parse mode.
func escapeHTML(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}
