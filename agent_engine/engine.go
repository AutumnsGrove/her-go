// Package agent_engine provides a shared tool-calling loop for all agentic
// agents (driver, memory, introspection, worker, dream). Each agent builds an
// EngineConfig with its dependencies and hook functions, then calls RunLoop().
// The engine owns the loop skeleton — iteration, continuation windows, tracing,
// TUI events, metrics — while agents own their identity, prompt construction, and
// behavioral hooks via nil-safe callbacks.
//
// The loop itself runs on github.com/AutumnsGrove/nova's agent package (see
// nova.go). This package owns the contract, not the mechanics: EngineConfig and
// LoopResult are her-go's, which is what let the implementation change without
// touching any of the five agents.
package agent_engine

import (
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

	// HookTimeout bounds each hook call. A hook that exceeds it is abandoned and
	// its return value discarded, so it must comfortably cover the slowest hook
	// (database writes under contention). 0 = defaultHookTimeout.
	HookTimeout time.Duration

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
)

// ---------------------------------------------------------------------------
// Defaults and caps
// ---------------------------------------------------------------------------

const (
	defaultIterationsPerWindow = 15
	maxIterationsPerWindowCap  = 50
	defaultMaxContinuations    = 2
	maxContinuationsCap        = 10

	// defaultHookTimeout bounds each hook call (see EngineConfig.HookTimeout).
	defaultHookTimeout = 2 * time.Minute
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

// RunLoop executes the shared tool-calling loop for one agent run.
//
// The implementation lives in nova.go and runs on github.com/AutumnsGrove/nova's
// agent loop. This wrapper is the seam the five agents were written against: they
// build an EngineConfig and read a LoopResult, and nothing about that contract
// changed when the loop body moved. Keeping it also keeps the loop's published
// behaviour ("how many iterations, which exit reason, which trace lines") in one
// place rather than spread through the agents.
//
// The loop exits when:
//   - The done tool sets ToolCtx.DoneCalled
//   - The model returns no tool calls
//   - All continuation windows are exhausted
//   - An LLM error occurs (after the fallback model has been tried)
//   - A hook asks to stop (honoured at the end of the tool batch)
func RunLoop(cfg EngineConfig) (*LoopResult, error) {
	return runNova(cfg)
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

// extractThinkBlock finds and removes a <think>...</think> block from
// model output. Reasoning models (MiMo, DeepSeek R1) wrap their chain
// of thought in these tags before the actual response. Returns the
// extracted thought, the cleaned content, and whether a block was found.
func extractThinkBlock(content string) (thought, cleaned string, ok bool) {
	openTag := "<think>"
	closeTag := "</think>"

	openIdx := strings.Index(content, openTag)
	closeIdx := strings.Index(content, closeTag)
	if openIdx < 0 || closeIdx < 0 || closeIdx <= openIdx {
		return "", content, false
	}

	thought = strings.TrimSpace(content[openIdx+len(openTag) : closeIdx])
	// Everything before <think> + everything after </think>
	cleaned = strings.TrimSpace(content[:openIdx] + content[closeIdx+len(closeTag):])
	return thought, cleaned, thought != ""
}
