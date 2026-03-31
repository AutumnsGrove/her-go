package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"her/agent/layers"
	"her/compact"
	"her/config"
	"her/embed"
	"her/llm"
	"her/logger"
	"her/memory"
	"her/persona"
	"her/scrub"
	"her/search"
	"her/skills/loader"
	"her/tools"
	"her/tui"
	"her/weather"

	// Blank imports trigger init() registration for each tool's handler.
	// Same pattern as database drivers: import _ "github.com/lib/pq"
	// Each import causes the package's init() to run, which calls
	// tools.Register("name", Handle) to add the handler to the registry.
	_ "her/tools/create_reminder"
	_ "her/tools/create_schedule"
	_ "her/tools/delete_expense"
	_ "her/tools/delete_schedule"
	_ "her/tools/done"
	_ "her/tools/find_skill"
	_ "her/tools/get_current_time"
	_ "her/tools/list_schedules"
	_ "her/tools/no_action"
	_ "her/tools/query_expenses"
	_ "her/tools/recall_memories"
	_ "her/tools/remove_fact"
	_ "her/tools/reply_confirm"
	_ "her/tools/run_skill"
	_ "her/tools/save_fact"
	_ "her/tools/save_self_fact"
	_ "her/tools/scan_receipt"
	_ "her/tools/search_history"
	_ "her/tools/set_location"
	_ "her/tools/think"
	_ "her/tools/update_expense"
	_ "her/tools/update_fact"
	_ "her/tools/update_persona"
	_ "her/tools/update_schedule"
	_ "her/tools/use_tools"
	_ "her/tools/view_image"
)

// log is the package-level logger for the agent package.
var log = logger.WithPrefix("agent")

// Callback types and toolContext have been moved to the tools package
// (tools/context.go). The agent imports them as tools.Context,
// tools.StatusCallback, etc. See tools/context.go for documentation.

// defaultAgentPrompt is used as a fallback if agent_prompt.md can't be loaded.
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
	ChatLLM                   *llm.Client
	VisionLLM                 *llm.Client // vision language model — nil if not configured
	ClassifierLLM             *llm.Client // classifier for memory writes — nil if not configured
	Store                     *memory.Store
	EmbedClient               *embed.Client
	SimilarityThreshold       float64
	TavilyClient              *search.TavilyClient
	WeatherClient             *weather.Client
	Cfg                       *config.Config
	ScrubbedUserMessage       string
	ScrubVault                *scrub.Vault
	ConversationID            string
	TriggerMsgID              int64
	StatusCallback            tools.StatusCallback
	SendCallback              tools.SendCallback
	TTSCallback               tools.TTSCallback
	TraceCallback             tools.TraceCallback             // nil if traces disabled
	StageResetCallback        tools.StageResetCallback        // nil-safe — sends new placeholder after reply
	DeletePlaceholderCallback tools.DeletePlaceholderCallback // nil-safe — deletes orphan placeholder on exit
	SendConfirmCallback       tools.SendConfirmCallback       // nil-safe — confirmation buttons for destructive actions
	ReflectionThreshold       int
	RewriteEveryN             int
	ImageBase64               string           // base64-encoded image data (empty if no image)
	ImageMIME                 string           // MIME type of the image (e.g., "image/jpeg")
	OCRText                   string           // pre-flight OCR text extracted from the photo (empty if no image or OCR unavailable)
	EventBus                  *tui.Bus         // nil-safe — emits rich typed events for the TUI
	ConfigPath                string           // path to config.yaml — needed for persisting location changes via set_location
	SkillRegistry             *loader.Registry // nil-safe — skill discovery and execution
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
		cr, compactErr := compact.MaybeCompact(
			params.ChatLLM, params.Store, params.ConversationID,
			compactionMsgs, params.Cfg.Memory.MaxHistoryTokens,
			params.Cfg.Memory.ChatContextBudget,
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

	// Semantic search — find facts most relevant to what the user just said.
	// This is the core of v0.4: instead of showing the LLM ALL facts sorted
	// by importance, we embed the user's message and find the closest matches
	// via sqlite-vec KNN. The results go into the system prompt so the
	// conversational model has the right context without seeing everything.
	var relevantFacts []memory.Fact
	if params.EmbedClient != nil && params.Store.EmbedDimension > 0 {
		queryVec, err := params.EmbedClient.Embed(params.ScrubbedUserMessage)
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
		WeatherClient:       params.WeatherClient,
		RelevantFacts:       relevantFacts,
		ConversationSummary: conversationSummary,
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
		ChatLLM:                   params.ChatLLM,
		VisionLLM:                 params.VisionLLM,
		ClassifierLLM:             params.ClassifierLLM,
		TavilyClient:              params.TavilyClient,
		WeatherClient:             params.WeatherClient,
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
		SkillRegistry:             params.SkillRegistry,
		// Inject classifier hooks so tool handlers in tools/ can call the
		// classifier without importing agent (which would be circular).
		// The ClassifyWriteFunc wraps classifyMemoryWrite, and
		// RejectionMessageFunc wraps rejectionMessage — both defined here.
		ClassifyWriteFunc: func(writeType, content string, snippet []memory.Message) tools.ClassifyVerdict {
			return classifyMemoryWrite(params.ClassifierLLM, writeType, content, snippet)
		},
		RejectionMessageFunc: func(verdict tools.ClassifyVerdict) string {
			return rejectionMessage(verdict)
		},
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
	// No tool_choice forcing — the model decides, and we handle gracefully
	// when it doesn't cooperate.
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

	// nudgedToolUse tracks whether we've already retried with
	// tool_choice="required". We only nudge once — if the model still
	// doesn't call tools after the nudge, fall through to the text fallback.
	nudgedToolUse := false

	for i := 0; i < 10; i++ {
		resp, err := params.AgentLLM.ChatCompletionWithTools(messages, toolDefs)
		if err != nil {
			// The LLM client handles fallback automatically on retriable
			// errors (429, 500-503, timeout). If we still get an error here,
			// both primary and fallback failed — bail out of the agent loop.
			log.Error("LLM error (primary + fallback both failed)", "err", err)
			if tracing {
				traceLines = append(traceLines, fmt.Sprintf("❌ <b>error:</b> %s", truncateLog(err.Error(), 100)))
				sendTrace()
			}
			break
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
			Time: time.Now(), TurnID: params.TriggerMsgID, Iteration: i,
			PromptTokens: resp.PromptTokens, CompletionTokens: resp.CompletionTokens,
			CostUSD: resp.CostUSD, FinishReason: resp.FinishReason,
		})

		// --- Check finish_reason to decide how to proceed ---
		hasToolCalls := len(resp.ToolCalls) > 0

		if !hasToolCalls {
			if resp.Content != "" {
				trimmed := strings.TrimSpace(strings.ToLower(resp.Content))
				// If the agent just typed "done" as text instead of calling
				// the done tool, treat it as a done signal. MiniMax does this.
				if trimmed == "done" || trimmed == "done." {
					log.Info("  agent typed 'done' as text (treating as done signal)")
					break
				}

				// --- Nudge: retry with tool_choice="required" ---
				// Diffusion models (Mercury 2) sometimes skip tool-calling
				// on simple messages (greetings, short replies), returning
				// plain text instead. Rather than always forcing "required"
				// (which can cause garbage tool calls), we detect the miss
				// and retry once with "required" as a gentle nudge.
				if !nudgedToolUse {
					nudgedToolUse = true
					log.Warn("  agent skipped tools — retrying with tool_choice=required")
					if tracing {
						traceLines = append(traceLines, "🔄 <i>nudge: retrying with tool_choice=required</i>")
						sendTrace()
					}
					// Feed the agent's text back as context so it doesn't
					// lose its train of thought on the retry.
					messages = append(messages, llm.ChatMessage{
						Role:    "assistant",
						Content: resp.Content,
					})
					messages = append(messages, llm.ChatMessage{
						Role:    "user",
						Content: "You must use your tools to respond. Call the reply tool with an instruction for how to respond, then call done. Do not respond with plain text.",
					})
					resp, err = params.AgentLLM.ChatCompletionWithTools(messages, toolDefs, "required")
					if err != nil {
						log.Error("nudge LLM error", "err", err)
						break
					}
					params.Store.SaveMetric(resp.Model, resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, 0, params.TriggerMsgID)
					totalCost += resp.CostUSD
					log.Infof("  nudge: %d prompt + %d completion | $%.6f | finish=%s",
						resp.PromptTokens, resp.CompletionTokens, resp.CostUSD, resp.FinishReason)

					// Surface model fallback on the nudge call too.
					if tracing && resp.UsedFallback {
						traceLines = append(traceLines, fmt.Sprintf("⚡ <i>nudge fallback: %s</i>", resp.Model))
						sendTrace()
					}

					// Check if the nudge worked.
					hasToolCalls = len(resp.ToolCalls) > 0
					if tracing {
						if hasToolCalls {
							traceLines = append(traceLines, "✅ <i>nudge succeeded</i>")
						} else {
							traceLines = append(traceLines,
								fmt.Sprintf("❌ <i>nudge failed — model returned text: %s</i>",
									escapeHTML(truncateLog(resp.Content, 120))))
						}
						sendTrace()
					}
					if !hasToolCalls {
						log.Warn("  nudge failed — model still returned text, falling back")
						agentFinalText = resp.Content
						break
					}
					// Fall through to tool execution below.
				} else {
					log.Warnf("  agent returned text (nudge already attempted): %s", truncateLog(resp.Content, 200))
					agentFinalText = resp.Content
					break
				}
			} else {
				log.Info("  done (no actions)")
				break
			}
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
					break
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

			result := executeTool(tc, tctx)
			log.Infof("    → %s: %s", tc.Function.Name, truncateLog(result, 200))
			emit(tui.ToolCallEvent{
				Time: time.Now(), TurnID: params.TriggerMsgID,
				ToolName: tc.Function.Name,
				Args:     truncateLog(tc.Function.Arguments, 200),
				Result:   truncateLog(result, 200),
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

		// --- Post-search think nudge ---
		// Diffusion LLMs (Mercury) skip the think step after receiving
		// search results, going straight to reply without evaluating.
		// This causes bad answers — e.g. trusting a wrong search summary
		// without checking the actual results. Autoregressive models
		// (Trinity) naturally paused to think ~70% of the time.
		//
		// If this batch contained search results and the model didn't
		// include a think call, inject a prompt telling it to evaluate
		// before proceeding.
		hasSearchResult := false
		hasThinkCall := false
		for _, tc := range resp.ToolCalls {
			if tc.Function.Name == "web_search" || tc.Function.Name == "book_search" {
				hasSearchResult = true
			}
			if tc.Function.Name == "think" {
				hasThinkCall = true
			}
		}
		if hasSearchResult && !hasThinkCall && !tctx.DoneCalled {
			log.Info("  injecting post-search think nudge")
			messages = append(messages, llm.ChatMessage{
				Role:    "user",
				Content: "You just received search results. Before calling reply, call think to evaluate: are the results relevant? Do they actually answer the question? Is the AI-generated summary accurate compared to the source snippets? Only then call reply with the correct information.",
			})
		}

		// Exit when the agent explicitly signals it's done.
		// (The "done" trace line is already added by formatTraceLine above.)
		if tctx.DoneCalled {
			log.Info("  done signal received")
			break
		}

		// Also exit if finish_reason was "stop" even though tools were
		// present — some providers do this (the OpenCode #14972 bug).
		if resp.FinishReason == "stop" {
			log.Info("  finish_reason=stop after tool execution")
			break
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
			fallbackResult := execReply(instruction, tctx)
			if tctx.ReplyCount == 0 {
				log.Error("fallback reply failed", "result", fallbackResult)
				return nil, fmt.Errorf("agent failed to generate a reply")
			}
		} else {
			log.Warn("reply was never called, generating generic fallback")
			fallbackResult := execReply(`{"instruction":"The user sent a message. Respond naturally. Do not reference any interruption or claim you were cut off."}`, tctx)
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

	// --- Persona Evolution Triggers ---
	// These run AFTER the response has been sent to the user.
	// They go in a goroutine because they don't affect the current turn.
	//
	// The chain: facts accumulate → triggers reflection →
	//            reflections accumulate → triggers persona rewrite
	// No concept of "conversations" needed — just fact and reflection counts.
	go func() {
		// Trigger: Reflection — have enough new facts accumulated since the last reflection?
		if params.ReflectionThreshold > 0 {
			factCount, err := params.Store.FactCountSinceLastReflection()
			if err != nil {
				log.Error("checking fact count for reflection trigger", "err", err)
			} else if factCount >= params.ReflectionThreshold {
				log.Infof("  [persona] reflection triggered (%d facts, threshold: %d)", factCount, params.ReflectionThreshold)
				emit(tui.PersonaEvent{
					Time: time.Now(), TurnID: params.TriggerMsgID,
					Action: "reflection_triggered",
					Detail: fmt.Sprintf("%d facts (threshold: %d)", factCount, params.ReflectionThreshold),
				})

				if tracing {
					traceLines = append(traceLines, fmt.Sprintf("💭 <b>reflection</b> triggered (%d new facts)", factCount))
					sendTrace()
				}

				// Gather the recent facts for the reflection prompt.
				recentFacts, _ := params.Store.RecentFacts("user", factCount)
				var factStrings []string
				for _, f := range recentFacts {
					factStrings = append(factStrings, f.Fact)
				}

				if err := persona.Reflect(params.ChatLLM, params.Store, params.ScrubbedUserMessage, tctx.ReplyText, factStrings, params.Cfg.Identity.Her, params.Cfg.Identity.User); err != nil {
					log.Error("reflection error", "err", err)
					if tracing {
						traceLines = append(traceLines, fmt.Sprintf("❌ <b>reflection</b> failed: %s", escapeHTML(truncateLog(err.Error(), 80))))
						sendTrace()
					}
				} else if tracing {
					traceLines = append(traceLines, "💭 <b>reflection</b> saved")
					sendTrace()
				}
			}
		}

		// Trigger: Persona rewrite — have enough reflections accumulated?
		// Rewrites fire at N, 2N, 3N, ... reflections (e.g. 3, 6, 9).
		// We check: totalReflections >= (rewrites+1) * threshold.
		// This way each rewrite "consumes" a batch and won't re-trigger
		// until the next batch accumulates.
		if params.RewriteEveryN > 0 {
			totalReflections, err := params.Store.TotalReflectionCount()
			if err != nil {
				log.Error("checking reflection count for rewrite trigger", "err", err)
			} else {
				rewriteCount, err := params.Store.PersonaRewriteCount()
				if err != nil {
					log.Error("checking persona rewrite count", "err", err)
				} else {
					nextThreshold := (rewriteCount + 1) * params.RewriteEveryN
					if totalReflections >= nextThreshold {
						log.Infof("  [persona] rewrite triggered (%d reflections, next threshold: %d)", totalReflections, nextThreshold)
						emit(tui.PersonaEvent{
							Time: time.Now(), TurnID: params.TriggerMsgID,
							Action: "rewrite_triggered",
							Detail: fmt.Sprintf("%d reflections (next: %d)", totalReflections, nextThreshold),
						})

						if tracing {
							traceLines = append(traceLines, fmt.Sprintf("✨ <b>persona rewrite</b> triggered (%d reflections)", totalReflections))
							sendTrace()
						}

						if rewritten, err := persona.MaybeRewrite(params.ChatLLM, params.Store, params.Cfg.Persona.PersonaFile, 0, params.Cfg.Identity.Her); err != nil {
							log.Error("persona rewrite error", "err", err)
							if tracing {
								traceLines = append(traceLines, fmt.Sprintf("❌ <b>persona rewrite</b> failed: %s", escapeHTML(truncateLog(err.Error(), 80))))
								sendTrace()
							}
						} else if rewritten {
							log.Info("persona.md rewritten")
							if tracing {
								traceLines = append(traceLines, "✨ <b>persona rewritten</b>")
								sendTrace()
							}
						}
					}
				}
			}
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
	case "reply":
		// reply remains in agent — it builds the full chat prompt using
		// agent-internal functions (buildReplyMessages, sendReply, etc.).
		return execReply(tc.Function.Arguments, tctx)
	default:
		// All other tools are registered in tools/ subdirectories and
		// dispatched via the central registry. tools.Execute validates JSON
		// and returns a clear error for unknown tools.
		return tools.Execute(tc.Function.Name, tc.Function.Arguments, tctx)
	}
}

// --- Reply tool ---

// execReply is the most important tool. It builds the full conversational
// prompt (prompt.md + persona + memory + search context + history) and
// calls the chatLLM to generate the actual response the user sees.
func execReply(argsJSON string, tctx *tools.Context) string {
	// Reset fallback tracking from any previous reply call in this turn.
	// Without this, a fallback on reply #1 would incorrectly flag reply #2.
	tctx.ReplyUsedFallback = false
	tctx.ReplyModel = ""

	var args struct {
		Instruction string `json:"instruction"`
		Context     string `json:"context"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	// Build the system prompt using the layer registry.
	// Each layer (persona, traits, memory, mood, etc.) lives in its own
	// file under agent/layers/ and auto-registers via init().
	chatLayerCtx := &layers.LayerContext{
		Store:               tctx.Store,
		Cfg:                 tctx.Cfg,
		EmbedClient:         tctx.EmbedClient,
		WeatherClient:       tctx.WeatherClient,
		RelevantFacts:       tctx.RelevantFacts,
		ConversationSummary: tctx.ConversationSummary,
		ConversationID:      tctx.ConversationID,
		ScrubbedUserMessage: tctx.ScrubbedUserMessage,
		ExpenseContext:      tctx.ExpenseContext,
	}
	systemPrompt, chatLayerResults := layers.BuildAll(layers.StreamChat, chatLayerCtx)

	// Log chat prompt shape for observability.
	var chatTotalTokens int
	for _, lr := range chatLayerResults {
		chatTotalTokens += lr.Tokens
		if lr.Detail != "" {
			log.Infof("  [chat layer] %s: ~%d tokens (%s)", lr.Name, lr.Tokens, lr.Detail)
		} else {
			log.Infof("  [chat layer] %s: ~%d tokens", lr.Name, lr.Tokens)
		}
		// Pass injected facts observability to the TUI.
		if tctx.EventBus != nil {
			for _, f := range lr.InjectedFacts {
				args := fmt.Sprintf("#%d %s imp=%d", f.ID, f.Source, f.Importance)
				if f.Source == "semantic" {
					args = fmt.Sprintf("#%d %s imp=%d dist=%.2f", f.ID, f.Source, f.Importance, f.Distance)
				}
				tctx.EventBus.Emit(tui.ToolCallEvent{
					Time:     time.Now(),
					TurnID:   tctx.TriggerMsgID,
					ToolName: "fact→chat",
					Args:     args,
					Result:   truncateLog(f.Fact, 80),
				})
			}
		}
	}
	log.Infof("  chat system prompt total: ~%d tokens", chatTotalTokens)

	// Combine any accumulated search context with the explicit context parameter.
	fullContext := tctx.SearchContext
	if args.Context != "" {
		if fullContext != "" {
			fullContext += "\n\n"
		}
		fullContext += args.Context
	}

	// Build the message list for the conversational model.
	var llmMessages []llm.ChatMessage
	llmMessages = append(llmMessages, llm.ChatMessage{
		Role:    "system",
		Content: systemPrompt,
	})

	// Add conversation history so the model has context of the ongoing chat.
	recentMsgs, err := tctx.Store.RecentMessages(tctx.ConversationID, tctx.Cfg.Memory.RecentMessages)
	if err != nil {
		log.Error("reply: loading history", "err", err)
	} else {
		// prevDay tracks the calendar date of the last message we appended.
		// When consecutive messages cross a midnight boundary, we inject a
		// system message so the chat model knows the earlier context is
		// from a different day (prevents perseveration on stale topics).
		var prevDay time.Time

		for _, msg := range recentMsgs {
			// For continuation replies (2nd, 3rd, etc.), strip out this
			// turn's messages — the trigger message and any replies we
			// already sent. Without this, the model sees its own first
			// reply in history plus the same user message appended below,
			// thinks it already answered, and generates identical output.
			// We keep everything BEFORE this turn so the model still has
			// the broader conversation context.
			if tctx.ReplyCount > 0 && msg.ID >= tctx.TriggerMsgID {
				continue
			}

			// Day boundary detection — inject a separator when messages
			// cross midnight so the model treats earlier context as
			// "yesterday" rather than the active conversation topic.
			msgDate := time.Date(msg.Timestamp.Year(), msg.Timestamp.Month(), msg.Timestamp.Day(), 0, 0, 0, 0, msg.Timestamp.Location())
			if !prevDay.IsZero() && !msgDate.Equal(prevDay) {
				llmMessages = append(llmMessages, llm.ChatMessage{
					Role:    "system",
					Content: "--- the above messages are from a previous day ---",
				})
			}
			prevDay = msgDate

			content := msg.ContentScrubbed
			if content == "" {
				content = msg.ContentRaw
			}
			llmMessages = append(llmMessages, llm.ChatMessage{
				Role:    msg.Role,
				Content: content,
			})
		}
	}

	// Build the user message. Search context and the agent's instruction
	// go into a lightweight system note so they don't masquerade as user
	// speech (which confused some models and caused degenerate outputs).
	if args.Instruction != "" || fullContext != "" {
		var note strings.Builder
		if fullContext != "" {
			note.WriteString("The following reference material may be useful for your response — use it naturally, don't quote verbatim or mention that you searched unless appropriate:\n\n")
			note.WriteString(fullContext)
			note.WriteString("\n\n")
		}
		if args.Instruction != "" {
			note.WriteString("Guidance from the assistant's planning layer: ")
			note.WriteString(args.Instruction)
		}
		llmMessages = append(llmMessages, llm.ChatMessage{
			Role:    "system",
			Content: note.String(),
		})
	}
	llmMessages = append(llmMessages, llm.ChatMessage{
		Role:    "user",
		Content: tctx.ScrubbedUserMessage,
	})

	// Call the conversational model.
	start := time.Now()
	resp, err := tctx.ChatLLM.ChatCompletion(llmMessages)
	latencyMs := int(time.Since(start).Milliseconds())

	if err != nil {
		log.Error("reply: LLM error", "err", err)
		return fmt.Sprintf("error generating response: %v", err)
	}

	tctx.ReplyCost += resp.CostUSD
	tctx.ReplyUsedFallback = resp.UsedFallback
	tctx.ReplyModel = resp.Model
	log.Infof("  reply: %d prompt + %d completion = %d total | $%.6f | %dms",
		resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, latencyMs)
	if tctx.EventBus != nil {
		tctx.EventBus.Emit(tui.ReplyEvent{
			Time: time.Now(), TurnID: tctx.TriggerMsgID,
			Text:             truncateLog(resp.Content, 200),
			PromptTokens:     resp.PromptTokens,
			CompletionTokens: resp.CompletionTokens,
			TotalTokens:      resp.TotalTokens,
			CostUSD:          resp.CostUSD,
			LatencyMs:        latencyMs,
		})
	}

	// Guard against degenerate responses. If the chat model returned
	// something suspiciously short (< 5 chars) or repetitive, it was
	// likely rate-limited or glitching. These garbage responses poison
	// the conversation history if saved, causing a feedback loop where
	// every subsequent turn degenerates further (the "ohohoh" incident).
	if isDegenerate(resp.Content) {
		log.Warn("reply: degenerate response detected, retrying once", "content", truncateLog(resp.Content, 80))
		// One retry — if the model is genuinely down, the fallback
		// in the agent loop will catch it.
		resp, err = tctx.ChatLLM.ChatCompletion(llmMessages)
		if err != nil {
			log.Error("reply: retry LLM error", "err", err)
			return fmt.Sprintf("error generating response: %v", err)
		}
		if isDegenerate(resp.Content) {
			log.Error("reply: degenerate response on retry too", "content", truncateLog(resp.Content, 80))
			return "error: model returned a degenerate response. Try again in a moment."
		}
	}

	// Save the response to the database.
	respID, err := tctx.Store.SaveMessage("assistant", resp.Content, resp.Content, tctx.ConversationID)
	if err != nil {
		log.Error("reply: saving response", "err", err)
	}

	// Update token counts on both the user message and the response.
	if tctx.TriggerMsgID > 0 {
		tctx.Store.UpdateMessageTokenCount(tctx.TriggerMsgID, resp.PromptTokens)
	}
	if respID > 0 {
		tctx.Store.UpdateMessageTokenCount(respID, resp.CompletionTokens)
		tctx.Store.SaveMetric(resp.Model, resp.PromptTokens, resp.CompletionTokens, resp.TotalTokens, resp.CostUSD, latencyMs, respID)
	}

	// Deanonymize PII tokens before sending to the user.
	// The LLM might have used placeholders like [PHONE_1] in its response —
	// we swap those back to the real values before the user sees it.
	replyText := scrub.Deanonymize(resp.Content, tctx.ScrubVault)

	// Duplicate reply guard — if the agent calls reply twice with the
	// same (or very similar) text, skip the second one. Trinity sometimes
	// loops think→reply→think→reply with identical content.
	if tctx.ReplyCalled && replyText == tctx.ReplyText {
		log.Warn("reply: duplicate detected, skipping")
		return "reply skipped (duplicate of previous reply)"
	}

	// Deliver the response to Telegram.
	// First reply: edit the placeholder message (statusCallback).
	// Follow-up replies: send as a new message (sendCallback) so both
	// are visible — e.g., "let me look that up" → "here's what I found".
	if tctx.ReplyCalled && tctx.SendCallback != nil {
		// Follow-up reply — send as a new message.
		if err := tctx.SendCallback(replyText); err != nil {
			log.Error("reply: sending follow-up to Telegram", "err", err)
		}
	} else if tctx.StatusCallback != nil {
		// First reply — edit the placeholder.
		if err := tctx.StatusCallback(replyText); err != nil {
			log.Error("reply: sending to Telegram", "err", err)
		}
	}

	// Fire TTS immediately — don't wait for the agent loop to finish.
	// This runs in a goroutine so the agent can keep thinking/acting
	// while the voice memo is being synthesized and sent.
	if tctx.TTSCallback != nil {
		go tctx.TTSCallback(replyText)
	}

	tctx.ReplyCalled = true
	tctx.ReplyCount++
	tctx.ReplyText = replyText

	// Stage reset: send a new Telegram placeholder so that any follow-up
	// work (search status updates, additional replies) doesn't overwrite
	// the reply we just sent. After the reset, statusCallback targets the
	// new placeholder and replyCalled is cleared so the next reply edits
	// it instead of using sendCallback.
	if tctx.StageResetCallback != nil {
		if err := tctx.StageResetCallback(); err != nil {
			log.Warn("reply: stage reset failed", "err", err)
		} else {
			tctx.ReplyCalled = false
		}
	}

	return fmt.Sprintf("reply sent (%d chars)", len(replyText))
}

// isDegenerate detects garbage LLM outputs that would poison conversation
// history if saved. Catches single-character responses, excessive repetition
// (like "ohohohohoh..."), and empty responses. These typically happen when
// the model is rate-limited, overloaded, or in a degenerate loop.
func isDegenerate(text string) bool {
	trimmed := strings.TrimSpace(text)

	// Empty or extremely short — a real reply should be at least a
	// short sentence. Single words like "you", "ok", "hi" indicate
	// the chatLLM choked (rate limit, timeout, degenerate output).
	if len(trimmed) < 10 {
		return true
	}

	// Repetition detector: if any 2-4 character substring repeats to
	// fill most of the response, it's degenerate. We check by taking
	// a small prefix and seeing if repeating it reconstructs the text.
	if len(trimmed) > 20 {
		for patLen := 1; patLen <= 4; patLen++ {
			pat := trimmed[:patLen]
			repeated := strings.Repeat(pat, len(trimmed)/patLen+1)
			// If the repeated pattern matches at least 90% of the text,
			// it's a repetition loop.
			if len(repeated) >= len(trimmed) && repeated[:len(trimmed)] == trimmed {
				return true
			}
		}
	}

	return false
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
