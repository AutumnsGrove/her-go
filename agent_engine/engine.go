// Package agent_engine provides a shared tool-calling loop for all agentic
// agents (driver, memory, introspection, worker, dream). Each agent builds
// an EngineConfig with its dependencies and hook functions, then calls
// RunLoop(). The engine owns the loop skeleton — iteration, continuation
// windows, tracing, TUI events, metrics — while agents own their identity,
// prompt construction, and behavioral hooks via nil-safe callbacks.
//
// This eliminates ~950 lines of duplicated loop logic that was previously
// copy-pasted across 5 agents in 4 packages.
package agent_engine

import (
	"fmt"
	"strings"
	"time"

	"her/llm"
	"her/logger"
	"her/memory"
	"her/tools"
	"her/tui"
	"her/turn"
)

var log = logger.WithPrefix("engine")

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

// EngineConfig defines a single agent run. The engine owns the loop;
// the caller owns identity, prompt construction, and behavioral hooks.
type EngineConfig struct {
	// -- Identity --

	// Name identifies this agent for logging and TUI event routing.
	Name string

	// MetricRole is stored with each SaveMetric call so cost dashboards
	// can break down spend per agent (e.g. memory.RoleDriver).
	MetricRole string

	// -- Core dependencies --

	// LLM is the client used for ChatCompletionWithTools calls.
	LLM *llm.Client

	// Store persists metrics. The engine only calls SaveMetric — all
	// other store operations happen inside tool handlers.
	Store memory.Store

	// ToolDefs is the set of tools available to the agent.
	ToolDefs []llm.ToolDef

	// ToolCtx is the pre-built context passed to every tools.Execute call.
	// The caller assembles this with agent-specific fields before calling
	// RunLoop. The engine reads ToolCtx.DoneCalled to detect the done signal.
	ToolCtx *tools.Context

	// Messages is the initial message list (typically [system, user]).
	// The engine appends to this slice during the loop.
	Messages []llm.ChatMessage

	// TriggerMsgID links metrics and TUI events to the originating message.
	TriggerMsgID int64

	// -- Loop tuning --

	// IterationsPerWindow is the max LLM calls per continuation window.
	// Default: 15. Hard cap: 50.
	IterationsPerWindow int

	// MaxContinuations is the number of extra windows the agent gets
	// after exhausting one. Default: 2. Hard cap: 10.
	MaxContinuations int

	// -- Observability (built-in, not hooks) --
	// The engine owns ALL trace emission. Agents never format or send
	// trace lines — the engine does it automatically via tools.FormatTrace().
	// This guarantees consistent formatting, HTML, and truncation.

	// TraceCallback sends trace content to the trace Board slot.
	// nil = tracing disabled for this agent.
	TraceCallback tools.TraceCallback

	// LiteToolHook fires with just the tool name after each execution.
	// Used by the lite trace system to build compact "tool1 → tool2" views.
	// nil = lite tracing disabled.
	LiteToolHook func(toolName string)

	// EventBus receives typed TUI events. nil-safe.
	EventBus *tui.Bus

	// Phase routes events through the turn tracker. nil-safe.
	// When set, events go through Phase (which auto-sets TurnID and Source).
	// When nil, events go directly to EventBus with Source set to Name.
	Phase *turn.PhaseHandle

	// -- Loop Lifecycle Hooks (all nil-safe) --

	// OnLoopStart fires once before the first LLM call. Use for setup,
	// logging "agent starting", injecting initial context, or starting
	// timers. Receives the initial messages slice.
	OnLoopStart func(messages []llm.ChatMessage)

	// OnLoopExit fires when the loop ends for any reason. Receives the exit
	// reason string and the final message history.
	// Use case: driver fallback reply path, placeholder cleanup.
	OnLoopExit func(reason string, messages []llm.ChatMessage)

	// OnDone fires specifically when the done tool sets DoneCalled=true,
	// before the loop breaks. Receives the final message history. Distinct
	// from OnLoopExit which fires for ALL exit reasons.
	// Use case: capture done summary, validate completion, final bookkeeping.
	OnDone func(messages []llm.ChatMessage)

	// OnError fires when the LLM returns an error (both primary and fallback
	// failed). Return true to suppress the default "break outer" behavior
	// (e.g., to retry with different parameters or inject a recovery message).
	// Use case: custom error recovery, alerting, retry logic.
	OnError func(err error, iteration, window int) (suppress bool)

	// -- Iteration Hooks (all nil-safe) --

	// PreIteration fires before each LLM call.
	// Use case: introspection agent latency tracking, token budget checks.
	PreIteration func(iteration, window int)

	// PostIteration fires after each LLM response, before tool execution.
	// Return true to break the loop.
	// Use case: driver loop detection (repeated think calls).
	PostIteration func(iteration, window int, resp *llm.ChatResponse) (breakLoop bool)

	// OnNoToolCalls fires when the LLM returns a response with no tool calls.
	// Return true to suppress the default "break outer" behavior (e.g., to
	// inject a retry message instead).
	// Use case: driver "done" text detection, agentFinalText capture.
	OnNoToolCalls func(resp *llm.ChatResponse) (handled bool)

	// -- Tool Hooks (all nil-safe) --

	// ToolChoiceFirst is passed as tool_choice on iteration 0 of window 0.
	// Typically "required" for the driver agent to force the model into the
	// tool-calling flow. nil = "auto" on every iteration.
	ToolChoiceFirst interface{}

	// ActiveToolGuard validates a tool call before execution. Return errResult
	// to reject — the engine appends it as an error response. nil = all
	// registered tools are allowed.
	// Use case: driver ActiveTools whitelist enforcement.
	ActiveToolGuard func(tc llm.ToolCall) (errResult string, reject bool)

	// PreTool fires before each tool execution. Return skip=true to prevent
	// execution — the engine appends skipResult as the tool response instead.
	// Use case: dream agent dry-run interception, maxOps safety cap.
	PreTool func(tc llm.ToolCall, tctx *tools.Context) (skipResult string, skip bool)

	// PostToolResult fires after tool execution but BEFORE the result is
	// appended to the message history. The returned string replaces the
	// result in the messages. Return the original result unchanged to pass
	// through. This is the only hook that can MUTATE the conversation.
	// Use case: PII redaction from tool output, metadata injection, error
	// rewriting, result transformation.
	PostToolResult func(tc llm.ToolCall, result string, isError bool) string

	// PostTool fires after each tool execution and after the result has been
	// appended to messages. AFTER the engine has already handled tracing,
	// lite trace, TUI events, and PostToolResult. This hook is observe-only
	// for agent-specific concerns: SaveAgentTurn, think trace capture,
	// operation counting, etc.
	PostTool func(tc llm.ToolCall, result string, isError bool)

	// -- Continuation Hooks (all nil-safe) --

	// ContinuationMsg builds the system message injected when a continuation
	// window opens. Receives window index (1-based), max windows, and a
	// plain-text summary of progress so far. nil = engine uses a sensible
	// default message.
	ContinuationMsg func(window, maxWindows int, summary string) string
}

// ---------------------------------------------------------------------------
// Result
// ---------------------------------------------------------------------------

// LoopResult captures everything the engine produced during the run.
// Callers map fields from this to their own result types.
type LoopResult struct {
	// Messages is the final message history (for post-loop inspection).
	Messages []llm.ChatMessage

	// TotalCost is the accumulated LLM cost across all iterations.
	TotalCost float64

	// ToolCalls is the total number of tool executions.
	ToolCalls int

	// Iterations is the total number of LLM calls made.
	Iterations int

	// ExitReason describes why the loop stopped.
	ExitReason string

	// TraceLines contains the accumulated trace lines (empty if tracing
	// was disabled). Useful for post-loop inspection or summary building.
	TraceLines []string
}

// Exit reason constants.
const (
	ExitDone             = "done"
	ExitNoToolCalls      = "no_tool_calls"
	ExitMaxContinuations = "max_continuations"
	ExitError            = "error"
	ExitHookBreak        = "hook_break"
	ExitFinishReasonStop = "finish_reason_stop"
)

// ---------------------------------------------------------------------------
// Defaults and caps
// ---------------------------------------------------------------------------

const (
	defaultIterationsPerWindow = 15
	maxIterationsPerWindowCap  = 50
	defaultMaxContinuations    = 2
	maxContinuationsCap        = 10
)

// coerce returns val if it's in [1, cap], otherwise returns def.
func coerce(val, def, cap int) int {
	if val <= 0 {
		return def
	}
	if val > cap {
		return cap
	}
	return val
}

// ---------------------------------------------------------------------------
// RunLoop
// ---------------------------------------------------------------------------

// RunLoop executes the shared tool-calling loop. The caller builds an
// EngineConfig with all dependencies and hooks, then RunLoop drives the
// iteration: LLM call → tool execution → trace/event emission → repeat.
//
// The loop exits when:
//   - The done tool sets ToolCtx.DoneCalled
//   - The LLM returns no tool calls (and OnNoToolCalls doesn't handle it)
//   - All continuation windows are exhausted
//   - An LLM error occurs
//   - A hook requests a break
//   - The LLM returns finish_reason="stop" after tool execution
func RunLoop(cfg EngineConfig) (*LoopResult, error) {
	iterationsPerWindow := coerce(cfg.IterationsPerWindow, defaultIterationsPerWindow, maxIterationsPerWindowCap)
	maxContinuations := coerce(cfg.MaxContinuations, defaultMaxContinuations, maxContinuationsCap)

	messages := cfg.Messages
	var totalCost float64
	var totalTools int
	var totalIters int
	var traceLines []string
	tracing := cfg.TraceCallback != nil

	// sendTrace pushes accumulated trace lines to the callback.
	// Called after every tool call so the user sees incremental progress.
	sendTrace := func() {
		if !tracing || len(traceLines) == 0 {
			return
		}
		if err := cfg.TraceCallback(strings.Join(traceLines, "\n")); err != nil {
			log.Warn("trace: failed to send/update", "err", err)
		}
	}

	// emit sends a TUI event through Phase or EventBus (nil-safe).
	emit := func(e tui.Event) {
		if cfg.Phase != nil {
			cfg.Phase.Emit(e)
		} else if cfg.EventBus != nil {
			cfg.EventBus.Emit(e)
		}
	}

	exitReason := ExitMaxContinuations

	// >> HOOK: OnLoopStart (fires once before first LLM call)
	if cfg.OnLoopStart != nil {
		cfg.OnLoopStart(messages)
	}

outer:
	for window := 0; window <= maxContinuations; window++ {

		// -- Continuation window injection --
		if window > 0 {
			summary := BuildContinuationSummary(traceLines)
			var contMsg string
			if cfg.ContinuationMsg != nil {
				contMsg = cfg.ContinuationMsg(window, maxContinuations, summary)
			} else {
				contMsg = fmt.Sprintf(
					"You have used all %d iterations in the previous window without calling done. "+
						"Continuation window %d of %d. Your progress so far:\n%s\n\n"+
						"Continue your work and call done when finished.",
					iterationsPerWindow, window, maxContinuations, summary,
				)
			}
			messages = append(messages, llm.ChatMessage{
				Role:    "system",
				Content: contMsg,
			})
			log.Infof("  [%s] continuation window %d/%d", cfg.Name, window, maxContinuations)

			if tracing {
				traceLines = append(traceLines, fmt.Sprintf(
					"🔄 <i>continuation window %d/%d</i>", window, maxContinuations))
				sendTrace()
			}
		}

		for i := 0; i < iterationsPerWindow; i++ {
			totalIters++

			// >> HOOK: PreIteration
			if cfg.PreIteration != nil {
				cfg.PreIteration(i, window)
			}

			// Tool choice — ToolChoiceFirst on the very first iteration only.
			var toolChoice interface{}
			if i == 0 && window == 0 && cfg.ToolChoiceFirst != nil {
				toolChoice = cfg.ToolChoiceFirst
			}

			// -- LLM call --
			resp, err := cfg.LLM.ChatCompletionWithTools(messages, cfg.ToolDefs, toolChoice)
			if err != nil {
				log.Error("LLM error (primary + fallback both failed)",
					"agent", cfg.Name, "err", err)
				if tracing {
					traceLines = append(traceLines, fmt.Sprintf(
						"❌ <b>error:</b> %s", TruncateLog(err.Error(), 100)))
					sendTrace()
				}

				// >> HOOK: OnError (can suppress the default break)
				if cfg.OnError != nil {
					if cfg.OnError(err, i, window) {
						continue // hook suppressed — retry or recover
					}
				}

				exitReason = ExitError
				break outer
			}

			// -- Metrics --
			if cfg.Store != nil {
				_ = cfg.Store.SaveMetric(
					resp.Model, resp.PromptTokens, resp.CompletionTokens,
					resp.TotalTokens, resp.CostUSD, 0, cfg.TriggerMsgID,
					resp.UsedFallback, cfg.MetricRole,
				)
			}
			totalCost += resp.CostUSD
			log.Infof("  [%s] tokens: %d prompt + %d completion | $%.6f | finish=%s",
				cfg.Name, resp.PromptTokens, resp.CompletionTokens,
				resp.CostUSD, resp.FinishReason)

			// -- Emit iteration event --
			emit(tui.AgentIterEvent{
				Time:             time.Now(),
				TurnID:           cfg.TriggerMsgID,
				Iteration:        totalIters - 1,
				PromptTokens:     resp.PromptTokens,
				CompletionTokens: resp.CompletionTokens,
				CostUSD:          resp.CostUSD,
				FinishReason:     resp.FinishReason,
			})

			// Surface model fallback in traces.
			if tracing && resp.UsedFallback {
				traceLines = append(traceLines, fmt.Sprintf(
					"⚡ <i>fallback: %s</i>", resp.Model))
				sendTrace()
			}

			// >> HOOK: PostIteration (before tool execution)
			if cfg.PostIteration != nil {
				if cfg.PostIteration(i, window, resp) {
					exitReason = ExitHookBreak
					break outer
				}
			}

			// -- No tool calls → exit (unless hook handles it) --
			if len(resp.ToolCalls) == 0 {
				if cfg.OnNoToolCalls != nil {
					if cfg.OnNoToolCalls(resp) {
						continue
					}
				}
				exitReason = ExitNoToolCalls
				break outer
			}

			// -- Append assistant message with tool calls --
			messages = append(messages, llm.ChatMessage{
				Role:      "assistant",
				Content:   resp.Content,
				ToolCalls: resp.ToolCalls,
			})

			// -- Execute each tool call --
			for _, tc := range resp.ToolCalls {
				// Guard: skip tool calls with empty names. Some providers
				// occasionally emit phantom tool call deltas.
				if tc.Function.Name == "" {
					log.Warn("skipping tool call with empty name",
						"agent", cfg.Name, "id", tc.ID)
					continue
				}

				// >> HOOK: ActiveToolGuard
				if cfg.ActiveToolGuard != nil {
					if errResult, reject := cfg.ActiveToolGuard(tc); reject {
						messages = append(messages, llm.ChatMessage{
							Role:       "tool",
							Content:    errResult,
							ToolCallID: tc.ID,
						})
						continue
					}
				}

				// >> HOOK: PreTool (dry-run interception, safety caps)
				var result string
				if cfg.PreTool != nil {
					if skipResult, skip := cfg.PreTool(tc, cfg.ToolCtx); skip {
						result = skipResult
					} else {
						result = tools.Execute(tc.Function.Name, tc.Function.Arguments, cfg.ToolCtx)
					}
				} else {
					result = tools.Execute(tc.Function.Name, tc.Function.Arguments, cfg.ToolCtx)
				}

				totalTools++
				isError := strings.HasPrefix(result, "error:") ||
					strings.HasPrefix(result, "rejected:")

				// == BUILT-IN: Trace emission ==
				// Every agent gets consistent formatting via tools.FormatTrace.
				if tracing {
					line := tools.FormatTrace(tc.Function.Name, tc.Function.Arguments, result)
					traceLines = append(traceLines, line)
					sendTrace()
				}

				// == BUILT-IN: Lite trace ==
				if cfg.LiteToolHook != nil {
					cfg.LiteToolHook(tc.Function.Name)
				}

				// == BUILT-IN: TUI event emission ==
				if cfg.Phase != nil {
					cfg.Phase.EmitToolCall(
						tc.Function.Name,
						TruncateLog(tc.Function.Arguments, 200),
						TruncateLog(result, 200),
						isError,
					)
				} else {
					emit(tui.ToolCallEvent{
						Time:     time.Now(),
						TurnID:   cfg.TriggerMsgID,
						Source:   cfg.Name,
						ToolName: tc.Function.Name,
						Args:     TruncateLog(tc.Function.Arguments, 200),
						Result:   TruncateLog(result, 200),
						IsError:  isError,
					})
				}

				// >> HOOK: PostToolResult (MUTABLE — can transform the result)
				// This is the only hook that can change what goes into
				// the message history. Fires before message append.
				if cfg.PostToolResult != nil {
					result = cfg.PostToolResult(tc, result, isError)
					// Re-check error status since the transform may have changed it.
					isError = strings.HasPrefix(result, "error:") ||
						strings.HasPrefix(result, "rejected:")
				}

				// >> HOOK: PostTool (observe-only: SaveAgentTurn, think capture, etc.)
				if cfg.PostTool != nil {
					cfg.PostTool(tc, result, isError)
				}

				// Append tool result to message history.
				messages = append(messages, llm.ChatMessage{
					Role:       "tool",
					Content:    result,
					ToolCallID: tc.ID,
				})
			}

			// Exit when the agent explicitly signals done.
			if cfg.ToolCtx.DoneCalled {
				// >> HOOK: OnDone (fires specifically on done signal)
				if cfg.OnDone != nil {
					cfg.OnDone(messages)
				}
				exitReason = ExitDone
				break outer
			}

			// Also exit on finish_reason=stop after tool execution —
			// some providers do this (the OpenCode #14972 pattern).
			if resp.FinishReason == "stop" {
				exitReason = ExitFinishReasonStop
				break outer
			}
		}

		// Inner loop exhausted without done. If at the hard cap, give up.
		if window == maxContinuations {
			log.Warn("hit max continuations without done signal",
				"agent", cfg.Name,
				"total_calls", iterationsPerWindow*(window+1))
			if tracing {
				traceLines = append(traceLines, "⚠️ <i>max continuations reached</i>")
				sendTrace()
			}
			exitReason = ExitMaxContinuations
			break outer
		}
	}

	// >> HOOK: OnLoopExit
	if cfg.OnLoopExit != nil {
		cfg.OnLoopExit(exitReason, messages)
	}

	return &LoopResult{
		Messages:   messages,
		TotalCost:  totalCost,
		ToolCalls:  totalTools,
		Iterations: totalIters,
		ExitReason: exitReason,
		TraceLines: traceLines,
	}, nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

// BuildContinuationSummary converts trace lines into a plain-text summary
// for injection into continuation window context. Strips HTML tags used for
// Telegram formatting so the model sees clean readable text. Capped at
// ~500 chars so it doesn't consume much of the agent's context window.
func BuildContinuationSummary(traceLines []string) string {
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

// TruncateLog collapses newlines and truncates a string for logging.
// Exported so agents can use it in their hooks (e.g. SaveAgentTurn).
func TruncateLog(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
