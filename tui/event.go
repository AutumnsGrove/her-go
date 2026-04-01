// Package tui provides a structured event system and interactive terminal UI
// for the her-go chatbot. Instead of flat log lines, packages emit typed events
// that the TUI renders as collapsible, navigable sections.
//
// The event system has two layers:
//   - LogEvent: a drop-in for log.Info/Warn/Error calls (the logger bridge
//     converts existing calls automatically)
//   - Typed events (TurnStartEvent, ToolCallEvent, etc.): carry rich structured
//     data so the TUI can render them intelligently — not just as text
//
// All events implement the Event interface and flow through the Bus.
package tui

import "time"

// Level mirrors charmbracelet/log levels so we don't depend on it directly.
// Think of this like Python's logging.DEBUG / logging.INFO / etc.
type Level int

const (
	LevelDebug Level = iota
	LevelInfo
	LevelWarn
	LevelError
	LevelFatal
)

// String returns a short label for the log level, used in file logging
// and as a fallback in the TUI.
func (l Level) String() string {
	switch l {
	case LevelDebug:
		return "DEBUG"
	case LevelInfo:
		return "INFO"
	case LevelWarn:
		return "WARN"
	case LevelError:
		return "ERROR"
	case LevelFatal:
		return "FATAL"
	default:
		return "???"
	}
}

// Event is the interface all events satisfy. The Bubble Tea model receives
// these as tea.Msg values via a channel bridge.
//
// This is like a Python protocol / abstract base class — any struct with
// these two methods counts as an Event. Go checks this at compile time
// (unlike Python's duck typing which checks at runtime).
type Event interface {
	EventTime() time.Time
	EventSource() string // "agent", "bot", "llm", "cmd", etc.
}

// ---------------------------------------------------------------------------
// Generic log event — replaces most log.Info/Warn/Error calls
// ---------------------------------------------------------------------------

// LogEvent is emitted by the logger bridge for every log.Info(), log.Warn(),
// log.Error() call across the codebase. It carries the same structured
// key-value pairs that charmbracelet/log supported.
type LogEvent struct {
	Time    time.Time
	Source  string
	Level   Level
	Message string
	Fields  map[string]any // structured key-value pairs, e.g. {"err": "timeout", "model": "deepseek"}
}

func (e LogEvent) EventTime() time.Time { return e.Time }
func (e LogEvent) EventSource() string  { return e.Source }

// ---------------------------------------------------------------------------
// Startup events — collapsible "Startup" section in the TUI
// ---------------------------------------------------------------------------

// StartupEvent tracks initialization progress. Each major subsystem
// (DB, LLM clients, sidecars, Telegram) emits one when it starts/finishes.
type StartupEvent struct {
	Time   time.Time
	Phase  string // "db", "llm", "agent", "vision", "embed", "stt", "tts", "skills", "proxy", "telegram", "scheduler"
	Status string // "starting", "ready", "skipped", "failed"
	Detail string // e.g. "model=deepseek-v3.2", "path=her.db"
}

func (e StartupEvent) EventTime() time.Time { return e.Time }
func (e StartupEvent) EventSource() string  { return "startup" }

// ---------------------------------------------------------------------------
// Message turn events — each incoming message becomes a collapsible section
// ---------------------------------------------------------------------------

// TurnStartEvent fires when a new user message arrives. Opens a new
// collapsible section in the TUI.
type TurnStartEvent struct {
	Time           time.Time
	TurnID         int64  // message ID from the database
	UserMessage    string // truncated for display
	ConversationID string
}

func (e TurnStartEvent) EventTime() time.Time { return e.Time }
func (e TurnStartEvent) EventSource() string  { return "bot" }

// AgentIterEvent fires once per agent loop iteration (up to 10 per turn).
// Each iteration is one LLM call to the agent model.
type AgentIterEvent struct {
	Time             time.Time
	TurnID           int64
	Iteration        int
	PromptTokens     int
	CompletionTokens int
	CostUSD          float64
	FinishReason     string // "tool_calls", "stop"
}

func (e AgentIterEvent) EventTime() time.Time { return e.Time }
func (e AgentIterEvent) EventSource() string  { return "agent" }

// ToolCallEvent fires when the agent executes a tool (think, reply,
// save_fact, web_search, etc.).
type ToolCallEvent struct {
	Time     time.Time
	TurnID   int64
	ToolName string
	Args     string // truncated JSON arguments
	Result   string // truncated result string
}

func (e ToolCallEvent) EventTime() time.Time { return e.Time }
func (e ToolCallEvent) EventSource() string  { return "agent" }

// ContextEvent fires after the agent builds its context (facts, semantic
// search results) at the start of a turn.
type ContextEvent struct {
	Time          time.Time
	TurnID        int64
	RelevantFacts int
}

func (e ContextEvent) EventTime() time.Time { return e.Time }
func (e ContextEvent) EventSource() string  { return "agent" }

// ReplyEvent fires when the reply tool completes — the chat model has
// generated the actual user-facing response.
type ReplyEvent struct {
	Time             time.Time
	TurnID           int64
	Text             string // truncated reply text
	PromptTokens     int
	CompletionTokens int
	TotalTokens      int
	CostUSD          float64
	LatencyMs        int
}

func (e ReplyEvent) EventTime() time.Time { return e.Time }
func (e ReplyEvent) EventSource() string  { return "agent" }

// TurnEndEvent fires when agent.Run() completes for a message turn.
// Updates the turn section header with totals.
type TurnEndEvent struct {
	Time       time.Time
	TurnID     int64
	TotalCost  float64
	ElapsedMs  int64
	ToolCalls  int
	FactsSaved int
}

func (e TurnEndEvent) EventTime() time.Time { return e.Time }
func (e TurnEndEvent) EventSource() string  { return "bot" }

// PersonaEvent fires for reflection and persona rewrite triggers.
type PersonaEvent struct {
	Time   time.Time
	TurnID int64
	Action string // "reflection_triggered", "reflection_saved", "rewrite_triggered", "rewrite_saved"
	Detail string
}

func (e PersonaEvent) EventTime() time.Time { return e.Time }
func (e PersonaEvent) EventSource() string  { return "persona" }

// ---------------------------------------------------------------------------
// Sidecar events — STT/TTS process output
// ---------------------------------------------------------------------------

// SidecarEvent captures stdout/stderr lines from the STT and TTS
// subprocess sidecars. Each gets its own collapsible section.
type SidecarEvent struct {
	Time    time.Time
	Sidecar string // "stt" or "tts"
	Line    string
	IsErr   bool // true = stderr, false = stdout
}

func (e SidecarEvent) EventTime() time.Time { return e.Time }
func (e SidecarEvent) EventSource() string  { return e.Sidecar }

// DDLEvent is emitted when a skill modifies its sidecar database schema.
// The audit skill (if installed) consumes these to decide whether to
// notify Autumn, log silently, or quarantine the skill.
type DDLEvent struct {
	Time      time.Time
	SkillName string
	Statement string // the DDL statement that was executed
}

func (e DDLEvent) EventTime() time.Time { return e.Time }
func (e DDLEvent) EventSource() string  { return "dbproxy" }

// CompactStartEvent fires when compaction begins — gives the TUI something
// to show while the LLM summarizes, instead of a dead zone where you can't
// tell if the bot is compacting, thinking, or crashed.
type CompactStartEvent struct {
	Time   time.Time
	Stream string // "chat" or "agent"
}

func (e CompactStartEvent) EventTime() time.Time { return e.Time }
func (e CompactStartEvent) EventSource() string  { return "compact" }

// CompactEvent fires when conversation history is compacted into a summary.
type CompactEvent struct {
	Time         time.Time
	Summarized   int // number of messages summarized
	TokensBefore int // estimated tokens before compaction
	TokensAfter  int // estimated tokens after compaction
}

func (e CompactEvent) EventTime() time.Time { return e.Time }
func (e CompactEvent) EventSource() string  { return "compact" }
