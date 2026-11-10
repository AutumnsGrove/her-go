// Nova-backed implementation of RunLoop.
//
// Seam 2 of the Nova migration (docs/plans/PLAN-nova-migration.md §3, step 7).
// `RunLoop` keeps its signature and its EngineConfig/LoopResult contract, so all
// five agents (driver, memory, introspection, worker, dream) keep working
// unchanged; only the loop's internals move. When `engine.backend` (or a
// per-agent override) says "nova", RunLoop dispatches here.
//
// The mapping is the interesting part, and it is not one-to-one:
//
//	EngineConfig field        Nova                                  note
//	----------------------    -----------------------------------   -------------------------------
//	Messages                  Config.System + Run(history, prompt)  first system message hoisted
//	IterationsPerWindow       MaxTurns = iters × (continuations+1)  windows become turn budgets
//	MaxContinuations          Hooks.TurnEnd injects a continuation   at each window boundary
//	ToolChoiceFirst           Hooks.BeforeProviderRequest           turn 0 only
//	PreIteration              Hooks.BeforeTurn
//	PostIteration             Hooks.MessageEnd                     cannot break (delta D1)
//	ActiveToolGuard, PreTool  wrapped tools (exact strings)         not Hooks.ToolCall, on purpose
//	PostToolResult, PostTool  Hooks.ToolResult
//	trace/lite/TUI per tool   Hooks.ToolResult
//	OnDone, ToolCtx.DoneCalled Hooks.TurnEnd → End: true
//	OnNoToolCalls             Hooks.MessageEnd                     cannot continue (delta D4)
//	OnLoopStart/OnLoopExit    before Run / Hooks.AgentEnd
//	SaveMetric, iterations    llm.Client ResponseHook              per provider call
//
// Reading order: novaRun (setup), then novaRunState's hook methods, then the
// small converters at the bottom.
package agent_engine

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/AutumnsGrove/nova/agent"
	"github.com/AutumnsGrove/nova/message"
	"github.com/AutumnsGrove/nova/provider"
	"github.com/AutumnsGrove/nova/tool"

	"her/llm"
	"her/memory"
	"her/tools"
	"her/tui"
)

// runNova executes one agent run on Nova's loop.
func runNova(cfg EngineConfig) (*LoopResult, error) {
	iterationsPerWindow := coerce(cfg.IterationsPerWindow, defaultIterationsPerWindow, maxIterationsPerWindowCap)
	maxContinuations := coerce(cfg.MaxContinuations, defaultMaxContinuations, maxContinuationsCap)

	if cfg.LLM == nil {
		return nil, fmt.Errorf("engine: LLM client is required")
	}
	if cfg.ToolCtx == nil {
		return nil, fmt.Errorf("engine: ToolCtx is required")
	}

	s := &novaRunState{
		cfg:                 cfg,
		iterationsPerWindow: iterationsPerWindow,
		maxContinuations:    maxContinuations,
		tracing:             cfg.TraceCallback != nil,
	}

	// Split her-go's [system, ...history, prompt] into Nova's three inputs.
	// Nova prepends Config.System itself, so leaving the system message in the
	// history as well would send it twice.
	system, history, prompt := splitMessages(cfg.Messages)

	// The registry must hold every tool this agent could ever call, not just the
	// ones advertised now: Nova dispatches through the registry, and use_tools
	// can only advertise a tool it can also run. The advertised subset is decided
	// per turn in beforeProviderRequest from ActiveTools.
	//
	// The union matters in both directions. cfg.ToolDefs carries the tools that
	// exist only in code rather than in a manifest — use_tools itself, and
	// reply_direct when the experiment flag is on — while ToolDefsForAgent carries
	// every deferred tool the agent is allowed to load. Using either alone broke
	// something real: the manifest-only set lost use_tools (the driver could never
	// load a tool), and the caller's set lost the deferred tools (Nova rejected
	// them as unknown the moment they were advertised).
	toolsForRun := unionToolDefs(cfg.ToolDefs, agentToolDefs(cfg))

	reg := tool.NewRegistry()
	for _, t := range tools.NovaTools(toolsForRun) {
		if err := reg.Register(guardedTool{inner: t, state: s}); err != nil {
			return nil, fmt.Errorf("engine: registering tool %q: %w", t.Info().Name, err)
		}
	}
	// The first request advertises exactly what the caller offered; later
	// requests follow ActiveTools (which use_tools grows).
	if cfg.ToolCtx != nil && cfg.ToolCtx.ActiveTools != nil && len(*cfg.ToolCtx.ActiveTools) > 0 {
		s.advertised = append([]llm.ToolDef(nil), (*cfg.ToolCtx.ActiveTools)...)
	} else {
		s.advertised = append([]llm.ToolDef(nil), cfg.ToolDefs...)
	}

	// Per-call metrics and iteration events. her-go records one metric row per
	// provider call; Nova's loop only sums usage per run, so the client hands
	// each finished response back through this hook. Restored afterwards
	// because a client can outlive one agent run.
	previousHook := cfg.LLM.ResponseHook
	cfg.LLM.ResponseHook = s.onResponse
	defer func() { cfg.LLM.ResponseHook = previousHook }()

	ag := agent.New(agent.Config{
		Provider: cfg.LLM, // *llm.Client implements provider.Provider (see llm/nova.go)
		Model:    cfg.LLM.Model(),
		System:   system,
		Tools:    reg,
		ToolData: cfg.ToolCtx,
		// Sequential execution preserves the old loop's guarantee: each tool
		// call sees the previous call's result (think → reply → done).
		ToolExecution: agent.Sequential,
		// One provider request per iteration, across all continuation windows.
		MaxTurns: iterationsPerWindow * (maxContinuations + 1),
		// Retries and the fallback model live in llm.Client (the retry
		// capability's owner). Nova retrying too would multiply attempts.
		Retry: agent.RetryPolicy{Max: -1},
		// No wrap-up request: the old loop simply stopped when the budget ran out.
		WrapUp:      nil,
		Temperature: cfg.LLM.TemperaturePtr(),
		MaxTokens:   cfg.LLM.MaxTokens(),
		Hooks: agent.Hooks{
			BeforeTurn:            s.beforeTurn,
			BeforeProviderRequest: s.beforeProviderRequest,
			MessageEnd:            s.messageEnd,
			ToolResult:            s.toolResult,
			TurnEnd:               s.turnEnd,
		},
	})

	if cfg.OnLoopStart != nil {
		log.Debug("hook fired", "agent", cfg.Name, "hook", "OnLoopStart")
		cfg.OnLoopStart(cfg.Messages)
	}

	res, err := ag.Run(context.Background(), history, prompt)
	if res == nil {
		if err == nil {
			err = fmt.Errorf("engine: nova run returned no result")
		}
		s.exitReason = ExitError
		if cfg.OnLoopExit != nil {
			cfg.OnLoopExit(s.exitReason, cfg.Messages)
		}
		return nil, err
	}

	s.result = res
	s.deriveExitReason(res)

	out := &LoopResult{
		Messages:   fromNovaMessages(res.Messages),
		TotalCost:  s.totalCost,
		ToolCalls:  s.toolCalls,
		Iterations: s.iterations,
		ExitReason: s.exitReason,
		TraceLines: s.traceLines,
	}

	if cfg.OnLoopExit != nil {
		log.Debug("hook fired", "agent", cfg.Name, "hook", "OnLoopExit", "reason", s.exitReason)
		cfg.OnLoopExit(s.exitReason, out.Messages)
	}

	// A provider failure that exhausted the fallback is returned as an error, as
	// the legacy loop returned nil after calling OnError. Callers treat both as
	// "the turn failed", and the metrics/exit reason are already recorded.
	if err != nil {
		return out, nil
	}
	return out, nil
}

// novaRunState carries everything the hooks need to close over. A struct with
// methods (rather than nested closures) keeps each hook readable and testable.
type novaRunState struct {
	cfg                 EngineConfig
	iterationsPerWindow int
	maxContinuations    int
	tracing             bool

	mu         sync.Mutex
	traceLines []string
	totalCost  float64
	toolCalls  int
	iterations int // provider requests so far
	window     int // continuation window index
	doneCalled bool
	// hookBreakRequested records that PostIteration asked to stop. The legacy
	// loop could break before executing the batch; Nova cannot, so the request is
	// honoured at the end of the batch instead (delta D1).
	hookBreakRequested bool
	lastNoTool         bool // the latest assistant message carried no tool calls
	// advertised is the tool set the next request should offer. Seeded from the
	// caller's ActiveTools (or ToolDefs) and refreshed from ActiveTools each turn.
	advertised []llm.ToolDef
	lastText   string
	exitReason string
	result     *agent.Result
}

// beforeTurn implements PreIteration: it fires before each provider request.
func (s *novaRunState) beforeTurn(_ context.Context, _ []message.Message) ([]message.Message, error) {
	s.mu.Lock()
	iteration := s.iterations
	s.mu.Unlock()

	s.window = iteration / s.iterationsPerWindow
	if s.cfg.PreIteration != nil {
		log.Debug("hook fired", "agent", s.cfg.Name, "hook", "PreIteration", "iter", iteration%s.iterationsPerWindow, "window", s.window)
		s.cfg.PreIteration(iteration%s.iterationsPerWindow, s.window)
	}
	return nil, nil // nil keeps the transcript
}

// beforeProviderRequest applies the two request-level behaviours the old loop
// owned: forcing tool_choice on the first request, and re-advertising tools that
// use_tools has since loaded.
func (s *novaRunState) beforeProviderRequest(_ context.Context, req *provider.Request) {
	s.mu.Lock()
	isFirst := s.iterations == 0
	s.mu.Unlock()

	// Deferred tool loading. use_tools appends to *ToolCtx.ActiveTools; the
	// legacy loop advertised only the tools it started with, so a loaded tool was
	// callable but invisible in the schema. Nova recomputes the tool list every
	// turn, so the freshly loaded schemas go out with the next request. This is
	// an intentional improvement — see the migration log, delta D9.
	//
	// Refresh first, then advertise, so a tool loaded during this turn's batch is
	// offered on the very next request.
	if s.cfg.ToolCtx != nil && s.cfg.ToolCtx.ActiveTools != nil && len(*s.cfg.ToolCtx.ActiveTools) > 0 {
		s.mu.Lock()
		s.advertised = append([]llm.ToolDef(nil), (*s.cfg.ToolCtx.ActiveTools)...)
		s.mu.Unlock()
	}

	s.mu.Lock()
	defs := s.advertised
	s.mu.Unlock()
	if converted, err := llm.NovaToolDefs(defs); err == nil && len(converted) > 0 {
		req.Tools = converted
	}

	if len(req.Tools) > 0 {
		// parallel_tool_calls:false on the wire, so a model that ignores the
		// parameter and batches calls anyway is still handled by the loop.
		f := false
		req.ParallelToolCalls = &f

		if isFirst && s.cfg.ToolChoiceFirst != nil {
			req.ToolChoice = s.cfg.ToolChoiceFirst
		}
	}
}

// messageEnd implements PostIteration (before tools run) plus the <think>
// extraction and the no-tool-calls signal.
//
// Delta D1: the legacy loop could *break* here — the driver's think-loop
// detector did. Nova's MessageEnd can only rewrite the message, not end the run.
// The driver's detector fires on a repeated `think`, which is a harmless no-op
// tool, so the run ends one turn later after that call. The parity test asserts
// it terminates.
func (s *novaRunState) messageEnd(_ context.Context, m message.Message) message.Message {
	s.mu.Lock()
	hasCalls := len(m.ToolCalls) > 0
	s.mu.Unlock()

	// <think> blocks: some MiMo builds put reasoning inline instead of in a
	// separate reasoning channel. Surface it as a synthetic think tool call so it
	// appears in traces exactly like the explicit think tool.
	if thought, cleaned, ok := extractThinkBlock(m.Content); ok {
		m.Content = cleaned
		s.emitThink(thought)
	}

	if s.cfg.PostIteration != nil {
		resp := &llm.ChatResponse{Content: m.Content, Reasoning: m.Reasoning, FinishReason: finishReasonFor(m)}
		if len(m.ToolCalls) > 0 {
			resp.ToolCalls = fromNovaToolCalls(m.ToolCalls)
		}
		s.mu.Lock()
		iteration, window := s.iterations, s.window
		s.mu.Unlock()
		if s.cfg.PostIteration(iteration%s.iterationsPerWindow, window, resp) {
			// Cannot end the run from here (delta D1). Record the intent so the
			// exit reason is honest, and let the batch run; TurnEnd ends it.
			s.mu.Lock()
			s.hookBreakRequested = true
			s.mu.Unlock()
		}
	}

	if !hasCalls {
		s.mu.Lock()
		s.lastNoTool = true
		s.lastText = m.Content
		s.mu.Unlock()

		// Delta D4: OnNoToolCalls cannot request another iteration here — Nova
		// ends the run when an assistant message has no tool calls. Only the
		// driver registers this hook, and only to capture the final text, so the
		// capture is preserved and the return value is ignored.
		if s.cfg.OnNoToolCalls != nil {
			s.cfg.OnNoToolCalls(&llm.ChatResponse{Content: m.Content, Reasoning: m.Reasoning, FinishReason: finishReasonFor(m)})
		}
	}
	return m
}

// toolResult implements the post-execution hooks in the same order the legacy
// loop used: trace, lite trace, TUI event, then the mutating PostToolResult,
// then the observe-only PostTool. Nova calls this before the result is recorded,
// which is exactly where the legacy PostToolResult fired.
func (s *novaRunState) toolResult(_ context.Context, c agent.ToolCall, r tool.Result) tool.Result {
	s.mu.Lock()
	s.toolCalls++
	s.mu.Unlock()

	tc := llm.ToolCall{
		ID:       c.ID,
		Type:     "function",
		Function: llm.FunctionCall{Name: c.Name, Arguments: string(c.Arguments)},
	}
	isError := tools.IsErrorResult(r.Content)

	if s.tracing {
		line := tools.FormatTrace(c.Name, string(c.Arguments), r.Content)
		s.mu.Lock()
		s.traceLines = append(s.traceLines, line)
		s.mu.Unlock()
		s.sendTrace()
	}

	if s.cfg.LiteToolHook != nil {
		s.cfg.LiteToolHook(c.Name)
	}

	if s.cfg.Phase != nil {
		s.cfg.Phase.EmitToolCall(c.Name, TruncateLog(string(c.Arguments), 200), TruncateLog(r.Content, 200), isError)
	} else {
		s.emit(tui.ToolCallEvent{
			Time:     time.Now(),
			TurnID:   s.cfg.TriggerMsgID,
			Source:   s.cfg.Name,
			ToolName: c.Name,
			Args:     TruncateLog(string(c.Arguments), 200),
			Result:   TruncateLog(r.Content, 200),
			IsError:  isError,
		})
	}

	if s.cfg.PostToolResult != nil {
		log.Debug("hook fired", "agent", s.cfg.Name, "hook", "PostToolResult", "tool", c.Name)
		r.Content = s.cfg.PostToolResult(tc, r.Content, isError)
		r.IsError = tools.IsErrorResult(r.Content)
	}

	if s.cfg.PostTool != nil {
		log.Debug("hook fired", "agent", s.cfg.Name, "hook", "PostTool", "tool", c.Name)
		s.cfg.PostTool(tc, r.Content, r.IsError)
	}

	return r
}

// turnEnd implements the end-of-batch decision: done, continuation windows, and
// the hook-break the legacy loop would have taken earlier.
func (s *novaRunState) turnEnd(_ context.Context, t agent.Turn) agent.TurnDecision {
	// The done tool sets DoneCalled on the shared context. End here, after the
	// batch (delta D1 accepts the extra batch).
	if s.cfg.ToolCtx.DoneCalled {
		s.mu.Lock()
		s.doneCalled = true
		s.mu.Unlock()
		if s.cfg.OnDone != nil {
			log.Debug("hook fired", "agent", s.cfg.Name, "hook", "OnDone")
			s.cfg.OnDone(fromNovaMessages(s.messagesSoFar(t)))
		}
		return agent.TurnDecision{End: true}
	}

	s.mu.Lock()
	hookBreak := s.hookBreakRequested
	s.hookBreakRequested = false
	s.mu.Unlock()
	if hookBreak {
		// The hook asked to stop; honour it now that the batch has run.
		s.mu.Lock()
		s.exitReason = ExitHookBreak
		s.mu.Unlock()
		return agent.TurnDecision{End: true}
	}

	// Continuation window boundary: the last iteration of a window has run and
	// it did not call done. Inject the continuation message (role: system, as the
	// legacy loop did) and keep going. Nova records it in the transcript and the
	// provider sends it like any other message (delta D2).
	iterationsDone := t.Index + 1
	if iterationsDone%s.iterationsPerWindow != 0 || iterationsDone/s.iterationsPerWindow > s.maxContinuations {
		return agent.TurnDecision{}
	}

	window := iterationsDone / s.iterationsPerWindow
	if window > s.maxContinuations {
		return agent.TurnDecision{}
	}

	s.mu.Lock()
	s.window = window
	summary := BuildContinuationSummary(s.traceLines)
	s.mu.Unlock()

	msg := ""
	if s.cfg.ContinuationMsg != nil {
		msg = s.cfg.ContinuationMsg(window, s.maxContinuations, summary)
	} else {
		msg = fmt.Sprintf(
			"You have used all %d iterations in the previous window without calling done. "+
				"Continuation window %d of %d. Your progress so far:\n%s\n\n"+
				"Continue your work and call done when finished.",
			s.iterationsPerWindow, window, s.maxContinuations, summary,
		)
	}
	log.Infof("  [%s] continuation window %d/%d", s.cfg.Name, window, s.maxContinuations)

	if s.tracing {
		s.mu.Lock()
		s.traceLines = append(s.traceLines, fmt.Sprintf("🔄 <i>continuation window %d/%d</i>", window, s.maxContinuations))
		s.mu.Unlock()
		s.sendTrace()
	}
	return agent.TurnDecision{Inject: []message.Message{message.System(msg)}}
}

// onResponse records the metrics and iteration event for one provider call.
// Installed on the llm.Client before the run and removed afterwards.
func (s *novaRunState) onResponse(info llm.ResponseInfo) {
	s.mu.Lock()
	s.totalCost += info.CostUSD
	s.iterations++
	iteration := s.iterations - 1
	s.mu.Unlock()

	if s.cfg.Store != nil {
		_ = s.cfg.Store.SaveMetric(memory.MetricInput{
			Model: info.Model, PromptTokens: info.PromptTokens, CompletionTokens: info.CompletionTokens,
			TotalTokens: info.TotalTokens, CostUSD: info.CostUSD, MessageID: s.cfg.TriggerMsgID,
			IsFallback: info.UsedFallback, AgentRole: s.cfg.MetricRole,
			CacheReadTokens: info.CacheReadTokens, CacheWriteTokens: info.CacheWriteTokens,
			Provider: info.Provider,
		})
	}
	log.Infof("  [%s] tokens: %d prompt + %d completion | $%.6f | finish=%s",
		s.cfg.Name, info.PromptTokens, info.CompletionTokens, info.CostUSD, info.FinishReason)

	s.emit(tui.AgentIterEvent{
		Time:             time.Now(),
		TurnID:           s.cfg.TriggerMsgID,
		Iteration:        iteration,
		PromptTokens:     info.PromptTokens,
		CompletionTokens: info.CompletionTokens,
		CostUSD:          info.CostUSD,
		FinishReason:     info.FinishReason,
		CacheReadTokens:  info.CacheReadTokens,
		CacheWriteTokens: info.CacheWriteTokens,
		Provider:         info.Provider,
	})

	if s.tracing && info.UsedFallback {
		s.mu.Lock()
		s.traceLines = append(s.traceLines, fmt.Sprintf("⚡ <i>fallback: %s</i>", info.Model))
		s.mu.Unlock()
		s.sendTrace()
	}
}

// deriveExitReason maps Nova's run outcome onto her-go's exit reasons, which
// callers and sim reports read.
func (s *novaRunState) deriveExitReason(res *agent.Result) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.exitReason != "" {
		return // a hook already decided
	}
	if s.doneCalled || res.Reason == "done" && !s.lastNoTool {
		s.exitReason = ExitDone
		return
	}
	switch res.Reason {
	case "done":
		s.exitReason = ExitNoToolCalls
	case "exhausted":
		s.exitReason = ExitMaxContinuations
	case "aborted", "error":
		s.exitReason = ExitError
	default:
		s.exitReason = ExitDone
	}
}

// messagesSoFar reconstructs the transcript at OnDone time. Nova only exposes
// the full transcript after the run, so this reports what the run had recorded
// up to the done call: the inputs plus the assistant message and its results.
func (s *novaRunState) messagesSoFar(t agent.Turn) []message.Message {
	out := append([]message.Message{}, s.appMessages()...)
	out = append(out, t.Message)
	for _, r := range t.Results {
		out = append(out, message.ToolResult("", r.Content))
	}
	return out
}

// appMessages is the initial transcript as Nova messages.
func (s *novaRunState) appMessages() []message.Message {
	msgs, _ := llm.NovaMessages(s.cfg.Messages)
	return msgs
}

// emit sends a TUI event through the phase handle when present, else the bus.
func (s *novaRunState) emit(e tui.Event) {
	if s.cfg.Phase != nil {
		s.cfg.Phase.Emit(e)
		return
	}
	if s.cfg.EventBus != nil {
		s.cfg.EventBus.Emit(e)
	}
}

// sendTrace pushes accumulated trace lines to the callback.
func (s *novaRunState) sendTrace() {
	if !s.tracing {
		return
	}
	s.mu.Lock()
	joined := strings.Join(s.traceLines, "\n")
	s.mu.Unlock()
	if joined == "" {
		return
	}
	if err := s.cfg.TraceCallback(joined); err != nil {
		log.Warn("trace: failed to send/update", "err", err)
	}
}

// emitThink records an inline <think> block as a synthetic think call.
func (s *novaRunState) emitThink(thought string) {
	if s.tracing {
		s.mu.Lock()
		s.traceLines = append(s.traceLines, fmt.Sprintf("🧠 <i>%s</i>", TruncateLog(thought, 300)))
		s.mu.Unlock()
		s.sendTrace()
	}
	if s.cfg.Phase != nil {
		s.cfg.Phase.EmitToolCall("think", TruncateLog(thought, 200), "", false)
	} else {
		s.emit(tui.ToolCallEvent{
			Time:     time.Now(),
			TurnID:   s.cfg.TriggerMsgID,
			Source:   s.cfg.Name,
			ToolName: "think",
			Args:     fmt.Sprintf(`{"thought":"%s"}`, TruncateLog(thought, 200)),
			Result:   "(internal reasoning)",
		})
	}
}

// guardedTool applies the engine's per-call hooks (ActiveToolGuard, PreTool)
// inside the tool call itself.
//
// The plan's step 6.1 puts these guards in the wrapper for one reason: Nova's
// own blocks produce different wording ("blocked: ...", "not approved: ..."),
// and prompts plus sims depend on the exact strings her-go has always returned
// ("error: malformed JSON in arguments...", "error: tool 'x' is not available").
// Wrapping keeps the bytes identical and keeps Nova's permission layer unused.
type guardedTool struct {
	inner tool.Tool
	state *novaRunState
}

func (g guardedTool) Info() tool.Info { return g.inner.Info() }

func (g guardedTool) Run(ctx context.Context, args json.RawMessage, tctx *tool.Context) (tool.Result, error) {
	tc := llm.ToolCall{
		ID:       tctx.CallID,
		Type:     "function",
		Function: llm.FunctionCall{Name: g.inner.Info().Name, Arguments: string(args)},
	}

	if guard := g.state.cfg.ActiveToolGuard; guard != nil {
		if errResult, reject := guard(tc); reject {
			log.Debug("hook rejected tool", "agent", g.state.cfg.Name, "hook", "ActiveToolGuard", "tool", tc.Function.Name)
			return tool.Result{Content: errResult, IsError: tools.IsErrorResult(errResult)}, nil
		}
	}

	if pre := g.state.cfg.PreTool; pre != nil {
		if skipResult, skip := pre(tc, g.state.cfg.ToolCtx); skip {
			log.Debug("hook skipped tool", "agent", g.state.cfg.Name, "hook", "PreTool", "tool", tc.Function.Name)
			return tool.Result{Content: skipResult, IsError: tools.IsErrorResult(skipResult)}, nil
		}
	}

	return g.inner.Run(ctx, args, tctx)
}

// ---------------------------------------------------------------------------
// Converters
// ---------------------------------------------------------------------------

// splitMessages turns her-go's [system, ...history, prompt] into Nova's three
// inputs. Nova prepends Config.System itself, so the system message must not
// stay in the history too.
func splitMessages(msgs []llm.ChatMessage) (system string, history []message.Message, prompt message.Message) {
	rest := msgs
	if len(rest) > 0 && rest[0].Role == "system" {
		system = rest[0].Content
		rest = rest[1:]
	}
	if len(rest) == 0 {
		return system, nil, message.User("")
	}
	last := rest[len(rest)-1]
	converted, err := llm.NovaMessages(rest[:len(rest)-1])
	if err != nil {
		// A conversion failure here means a message the transport could not have
		// sent either; keep the history empty rather than sending half of it.
		converted = nil
	}
	promptMsgs, err := llm.NovaMessages([]llm.ChatMessage{last})
	if err == nil && len(promptMsgs) == 1 {
		prompt = promptMsgs[0]
	} else {
		prompt = message.User(last.Content)
	}
	return system, converted, prompt
}

// fromNovaMessages converts a Nova transcript back to her-go's shape, because
// callers read LoopResult.Messages (extractDoneSummary, extractLastThink).
func fromNovaMessages(msgs []message.Message) []llm.ChatMessage {
	out := make([]llm.ChatMessage, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, llm.ChatMessage{
			Role:       string(m.Role),
			Content:    m.Content,
			ToolCalls:  fromNovaToolCalls(m.ToolCalls),
			ToolCallID: m.ToolCallID,
		})
	}
	return out
}

func fromNovaToolCalls(calls []message.ToolCall) []llm.ToolCall {
	if len(calls) == 0 {
		return nil
	}
	out := make([]llm.ToolCall, 0, len(calls))
	for _, c := range calls {
		out = append(out, llm.ToolCall{
			ID:       c.ID,
			Type:     c.Type,
			Function: llm.FunctionCall{Name: c.Function.Name, Arguments: c.Function.Arguments},
		})
	}
	return out
}

// finishReasonFor reports what the old loop would have seen. Nova rewrites a
// "stop" that arrived with tool calls to "tool_calls" (see its Collector), which
// is the one finish-reason behaviour that cannot be reproduced here — delta D3.
func finishReasonFor(m message.Message) string {
	if len(m.ToolCalls) > 0 {
		return "tool_calls"
	}
	return "stop"
}

// agentToolDefs returns every tool the agent may call according to the YAML
// manifests, or nil when the config is not reachable.
func agentToolDefs(cfg EngineConfig) []llm.ToolDef {
	if cfg.ToolCtx == nil || cfg.ToolCtx.Cfg == nil {
		return nil
	}
	return tools.ToolDefsForAgent(cfg.ToolCtx.AgentName, cfg.ToolCtx.Cfg)
}

// unionToolDefs merges definition lists, first mention of a name winning and
// order preserved. Registration order is the order the model sees, so keeping the
// caller's order first also keeps the prompt bytes stable across runs.
func unionToolDefs(lists ...[]llm.ToolDef) []llm.ToolDef {
	seen := map[string]bool{}
	var out []llm.ToolDef
	for _, list := range lists {
		for _, def := range list {
			if seen[def.Function.Name] {
				continue
			}
			seen[def.Function.Name] = true
			out = append(out, def)
		}
	}
	return out
}
