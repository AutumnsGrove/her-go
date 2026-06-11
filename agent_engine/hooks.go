// Package agent_engine — hooks.go defines the turn lifecycle hook types.
//
// These hooks fire at boundaries OUTSIDE the tool-calling loop — before
// any agent runs, after all agents finish, when a message is sent, etc.
// They live in the bot/orchestration layer, not inside RunLoop.
//
// The engine package defines the TYPES so there's one vocabulary for hooks
// across the entire system. The bot layer wires them up.
//
// Hook categories:
//   - Turn lifecycle: TurnStart, TurnEnd
//   - Message lifecycle: MessageSend
//   - Agent lifecycle: AgentStart, AgentEnd
//   - Background lifecycle: BackgroundStart, BackgroundEnd
package agent_engine

import (
	"her/llm"
)

// ---------------------------------------------------------------------------
// Turn Lifecycle Hooks
// ---------------------------------------------------------------------------

// TurnStartHook fires before any agent runs on a user message.
// Receives the raw input and can gate execution (return skip=true to
// abort the turn, e.g. substance check says "not worth analyzing").
//
// Current spaghetti this replaces:
//   - Substance gate classifier (bot/run_agent.go:424)
//   - Fast-path routing (bot/run_agent.go:220)
//   - Typing indicator (bot/run_agent.go:113)
//
// Multiple hooks can be chained. If any returns skip=true, the turn
// is aborted with the provided reason.
type TurnStartHook func(ctx TurnStartContext) (skip bool, reason string)

// TurnStartContext carries everything available at turn start.
type TurnStartContext struct {
	UserMessage    string
	ConversationID string
	TriggerMsgID   int64
	IsEvent        bool   // true if this turn was triggered by an agent event, not a user message
	EventType      string // "inbox_ready", "worker_complete", etc. (empty for user messages)
}

// TurnEndHook fires after ALL agents (main + background) have finished.
// This is the point where cost is finalized, traces are closed, and
// post-turn actions happen.
//
// Current spaghetti this replaces:
//   - TTS on reply (was inside reply tool via TTSCallback)
//   - Report URL append (ad-hoc in bot/telegram.go:696)
//   - Cost trace emission (bot/run_agent.go:494)
//   - Trace finalization (bot/run_agent.go:503)
//
// Multiple hooks can be chained. All fire (no gating — turn already happened).
type TurnEndHook func(ctx TurnEndContext)

// TurnEndContext carries everything available at turn end.
type TurnEndContext struct {
	UserMessage    string
	ReplyText      string
	ConversationID string
	TriggerMsgID   int64
	TotalCost      float64
	ToolCalls      int
	MemoriesSaved  int
	ExitReason     string // from the driver's LoopResult

	// Pending artifacts from the turn.
	PendingNarration   string // report text for voice memo
	PublishedReportURL string // Telegraph URL from publish_report
}

// ---------------------------------------------------------------------------
// Message Lifecycle Hooks
// ---------------------------------------------------------------------------

// MessageSendHook fires when a reply is delivered to the user.
// This is the natural place for TTS, message formatting, analytics, etc.
//
// Current spaghetti this replaces:
//   - Auto-TTS (confusing TTSCallback inside reply tool)
//   - Message pagination (SendPaginatedCallback)
//
// Multiple hooks can be chained. All fire (message is already sent by the
// time hooks run — these are post-delivery observers).
type MessageSendHook func(ctx MessageSendContext)

// MessageSendContext carries the delivered message details.
type MessageSendContext struct {
	Text           string // the reply text
	ConversationID string
	TriggerMsgID   int64
	IsFirstReply   bool   // true for the first reply in this turn (edits placeholder)
	IsContinuation bool   // true for follow-up replies after the first
	Model          string // which model generated this reply
	UsedFallback   bool   // true if the chat model fell back
	CostUSD        float64
}

// ---------------------------------------------------------------------------
// Agent Lifecycle Hooks
// ---------------------------------------------------------------------------

// AgentStartHook fires before a specific agent's RunLoop begins.
// Can skip the agent entirely (e.g., introspection pre-filter).
//
// Current spaghetti this replaces:
//   - Introspection pre-filter (agent/introspection_agent.go:96)
//   - Memory context snapshot (agent/memory_agent.go:106)
type AgentStartHook func(ctx AgentStartContext) (skip bool, reason string)

// AgentStartContext carries pre-agent state.
type AgentStartContext struct {
	AgentName      string // "driver", "memory", "introspection", "worker", "dream"
	ConversationID string
	TriggerMsgID   int64
	UserMessage    string
	ReplyText      string // empty for driver (hasn't replied yet)
}

// AgentEndHook fires after a specific agent's RunLoop returns.
// Observe-only — the agent has already finished.
//
// Current spaghetti this replaces:
//   - Memory result lite trace update (bot/run_agent.go:577)
//   - Introspection result trace (bot/run_agent.go)
type AgentEndHook func(ctx AgentEndContext)

// AgentEndContext carries post-agent state.
type AgentEndContext struct {
	AgentName     string
	ExitReason    string
	TotalCost     float64
	ToolCalls     int
	MemoriesSaved int
	Duration      int64 // milliseconds
}

// ---------------------------------------------------------------------------
// Background Agent Lifecycle Hooks
// ---------------------------------------------------------------------------

// BackgroundStartHook fires before the background agent batch launches
// (memory, mood, introspection). Can customize which agents run.
//
// Current spaghetti this replaces:
//   - Substance gate controls which agents fire (bot/run_agent.go:463)
//   - Batch threshold logic (bot/run_agent.go:454)
type BackgroundStartHook func(ctx BackgroundStartContext) (skip bool, reason string)

// BackgroundStartContext carries the batch state.
type BackgroundStartContext struct {
	ConversationID  string
	TurnCount       int    // accumulated turns since last background run
	ShouldAnalyze   bool   // substance gate verdict
	UserMessage     string
	ReplyText       string
	ThinkTraces     []string         // driver's reasoning for memory agent
	DriverMessages  []llm.ChatMessage // full driver conversation for context
}

// BackgroundEndHook fires after ALL background agents have completed.
//
// Current spaghetti this replaces:
//   - Cost finalization wait (bot/run_agent.go:494)
//   - Phase coordination cleanup
type BackgroundEndHook func(ctx BackgroundEndContext)

// BackgroundEndContext carries the aggregated results.
type BackgroundEndContext struct {
	ConversationID string
	MemoriesSaved  int
	SelfMemories   int
	MoodLogged     bool
	TotalCost      float64
}

// ---------------------------------------------------------------------------
// Hook Registry (optional future use)
// ---------------------------------------------------------------------------

// TurnHooks bundles all turn lifecycle hooks for easy wiring.
// The bot layer creates one of these and passes it through the pipeline.
// All fields are nil-safe — nil means "no hook at this point."
type TurnHooks struct {
	OnTurnStart       []TurnStartHook
	OnTurnEnd         []TurnEndHook
	OnMessageSend     []MessageSendHook
	OnAgentStart      []AgentStartHook
	OnAgentEnd        []AgentEndHook
	OnBackgroundStart []BackgroundStartHook
	OnBackgroundEnd   []BackgroundEndHook
}

// FireTurnStart runs all TurnStart hooks in order. Returns skip=true if
// any hook requests it (first skip wins).
func (h *TurnHooks) FireTurnStart(ctx TurnStartContext) (bool, string) {
	if h == nil {
		return false, ""
	}
	for _, hook := range h.OnTurnStart {
		if hook == nil {
			continue
		}
		if skip, reason := hook(ctx); skip {
			return true, reason
		}
	}
	return false, ""
}

// FireTurnEnd runs all TurnEnd hooks. None can gate — the turn already happened.
func (h *TurnHooks) FireTurnEnd(ctx TurnEndContext) {
	if h == nil {
		return
	}
	for _, hook := range h.OnTurnEnd {
		if hook == nil {
			continue
		}
		hook(ctx)
	}
}

// FireMessageSend runs all MessageSend hooks.
func (h *TurnHooks) FireMessageSend(ctx MessageSendContext) {
	if h == nil {
		return
	}
	for _, hook := range h.OnMessageSend {
		if hook == nil {
			continue
		}
		hook(ctx)
	}
}

// FireAgentStart runs all AgentStart hooks. Returns skip=true if any hook
// requests it (first skip wins).
func (h *TurnHooks) FireAgentStart(ctx AgentStartContext) (bool, string) {
	if h == nil {
		return false, ""
	}
	for _, hook := range h.OnAgentStart {
		if hook == nil {
			continue
		}
		if skip, reason := hook(ctx); skip {
			return true, reason
		}
	}
	return false, ""
}

// FireAgentEnd runs all AgentEnd hooks.
func (h *TurnHooks) FireAgentEnd(ctx AgentEndContext) {
	if h == nil {
		return
	}
	for _, hook := range h.OnAgentEnd {
		if hook == nil {
			continue
		}
		hook(ctx)
	}
}

// FireBackgroundStart runs all BackgroundStart hooks.
func (h *TurnHooks) FireBackgroundStart(ctx BackgroundStartContext) (bool, string) {
	if h == nil {
		return false, ""
	}
	for _, hook := range h.OnBackgroundStart {
		if hook == nil {
			continue
		}
		if skip, reason := hook(ctx); skip {
			return true, reason
		}
	}
	return false, ""
}

// FireBackgroundEnd runs all BackgroundEnd hooks.
func (h *TurnHooks) FireBackgroundEnd(ctx BackgroundEndContext) {
	if h == nil {
		return
	}
	for _, hook := range h.OnBackgroundEnd {
		if hook == nil {
			continue
		}
		hook(ctx)
	}
}
