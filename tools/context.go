// Package tools defines the registry and shared types for the agent's
// tool system. Each tool lives in its own subdirectory (tools/<name>/)
// with a YAML manifest (tool.yaml) and a Go handler (handler.go).
//
// The package avoids circular imports by sitting BETWEEN the agent and
// the individual tool packages:
//
//	agent  ──imports──►  tools/          (Context, Registry, Execute)
//	agent  ──imports──►  tools/reply/    (blank import triggers init registration)
//	tools/reply/  ──imports──►  tools/   (Context type, Register func)
//
// tools/ never imports agent. The agent pushes everything handlers need
// into tools.Context before calling Execute.
package tools

import (
	"her/config"
	"her/embed"
	"her/llm"
	"her/memory"
	"her/scrub"
	"her/search"
	"her/tui"
	"her/turn"
)

// ---------------------------------------------------------------------------
// Callback types — moved from agent so both packages can reference them.
//
// These are function signatures that the bot (Telegram layer) provides to
// the agent so tools can interact with the chat in real time. In Python
// you'd pass lambdas; in Go you declare the function signature as a type.
// ---------------------------------------------------------------------------

// StatusCallback updates the Telegram placeholder message with a status
// like "searching..." or "thinking...". The reply tool edits it to the
// final response.
type StatusCallback func(status string) error

// SendCallback sends a NEW Telegram message (as opposed to editing the
// placeholder). Used for follow-up replies — the first reply edits the
// placeholder, subsequent replies send new messages.
type SendCallback func(text string) error

// TTSCallback triggers voice synthesis. Runs in a goroutine so it
// doesn't block the agent loop.
type TTSCallback func(text string)

// TraceCallback sends or updates the agent thinking trace message.
// The agent builds up trace lines as it processes each tool call.
type TraceCallback func(text string) error

// StageResetCallback sends a fresh Telegram placeholder after a reply,
// so the next statusCallback targets a new message instead of
// overwriting the sent reply.
type StageResetCallback func() error

// SendConfirmCallback sends a message with Yes/No inline keyboard and
// returns the Telegram message ID. Used by reply_confirm for destructive
// action confirmation.
type SendConfirmCallback func(text string) (telegramMsgID int64, err error)

// DeletePlaceholderCallback removes the orphan placeholder left after
// the last stage reset when the agent loop exits.
type DeletePlaceholderCallback func() error

// StreamCallback delivers streaming chat tokens to the transport layer.
// Called once per token chunk during ChatCompletionStreaming. The reply
// tool calls this as the chat model streams — the bot layer batches these
// into Telegram edits for a live typing effect.
type StreamCallback func(chunk string) error

// AgentEventCallback fires a system event that can trigger an agent run
// without a user message. Used by notify_agent to wake up the main agent
// after the memory agent finishes background work.
//
// The callback is defined here (not in agent/) to avoid circular imports.
// The bot layer provides the implementation that translates these params
// into an agent.AgentEvent and writes it to the agent event channel.
type AgentEventCallback func(summary, directMessage string)

// ---------------------------------------------------------------------------
// Context — the dependency bundle every tool handler receives.
//
// This is the exported version of what used to be agent.toolContext.
// The agent constructs it at the start of each turn and passes it to
// every tool call via Execute(). Handlers pull what they need from it.
// ---------------------------------------------------------------------------

// Context bundles all dependencies that tool handlers need. The agent
// constructs one per conversation turn and passes it through to every
// tool call. Mutable fields (ReplyCalled, DoneCalled, etc.) track
// execution state across the agent loop.
type Context struct {
	// --- LLM clients ---

	// ChatLLM is the conversational model (configured via cfg.Chat). The reply
	// tool uses this to generate the user-facing response.
	ChatLLM *llm.Client

	// VisionLLM is the vision language model (e.g., Gemini Flash).
	// The view_image tool uses this. Nil if not configured.
	VisionLLM *llm.Client

	// ClassifierLLM validates memory writes (memories, moods, receipts)
	// before they hit the DB. Nil if not configured — writes pass
	// through unchanged.
	ClassifierLLM *llm.Client

	// --- Storage & search ---

	// Store is the SQLite-backed store for memories, messages, schedules,
	// expenses, and everything else that persists.
	Store *memory.Store

	// EmbedClient generates embedding vectors for semantic search.
	EmbedClient *embed.Client

	// TavilyClient provides web search and URL extraction. Nil if not
	// configured.
	TavilyClient *search.TavilyClient

	// --- Callbacks (all nil-safe) ---

	StatusCallback            StatusCallback
	SendCallback              SendCallback
	TTSCallback               TTSCallback
	TraceCallback             TraceCallback
	StageResetCallback        StageResetCallback
	DeletePlaceholderCallback DeletePlaceholderCallback
	SendConfirmCallback       SendConfirmCallback
	// StreamCallback delivers streaming tokens to Telegram in real time.
	// Nil means streaming is disabled for this turn — reply falls back to
	// the existing non-streaming path automatically.
	StreamCallback StreamCallback

	// StopTypingFn stops the Telegram typing indicator. Called by the
	// reply tool after successfully delivering a reply, so typing stops
	// immediately instead of lingering until agent.Run returns. Nil-safe.
	StopTypingFn func()

	// Phase is the turn phase handle for the current agent. Used by
	// tools to emit events (ReplyEvent, ToolCallEvent) that auto-route
	// to the correct turn and content group. Nil-safe — falls back to
	// direct EventBus emission when not set.
	Phase *turn.PhaseHandle

	// --- Conversation state ---

	// ScrubbedUserMessage is the PII-scrubbed version of what the user
	// said. Used by the reply tool when building the chat prompt.
	ScrubbedUserMessage string

	// ConversationID identifies the current conversation for history
	// retrieval.
	ConversationID string

	// TriggerMsgID is the DB message ID that started this agent run.
	// Used for linking metrics and saving the response.
	TriggerMsgID int64

	// ConversationSummary is the compacted summary of older messages.
	// Injected into the system prompt for context.
	ConversationSummary string

	// RelevantMemories are memories semantically similar to the user's message.
	// Used as fallback injection for the chat model when the agent didn't
	// explicitly pass memories via the reply tool's memories parameter.
	RelevantMemories []memory.Memory

	// AgentPassedMemories holds memories the agent explicitly chose to pass to the
	// reply tool via its memories parameter. When non-empty, the chat model uses
	// these instead of RelevantMemories — they represent the agent's curated
	// judgment of what's contextually relevant, not just message-hash similarity.
	AgentPassedMemories []string

	// SearchContext accumulates search results, book data, and URL
	// content across tool calls. Included in the reply prompt.
	SearchContext string

	// --- Image / OCR ---

	// ImageBase64 and ImageMIME hold the current photo data (if any).
	ImageBase64 string
	ImageMIME   string

	// OCRText holds pre-flight OCR text from the photo (if any).
	OCRText string

	// --- Execution state (mutable during agent loop) ---

	// ActiveTools points to the tools slice in the agent loop. The
	// use_tools handler appends deferred tools to it.
	ActiveTools *[]llm.ToolDef

	// ReplyCalled tracks whether reply has been called since the last
	// stage reset. Reset to false after each stage reset.
	ReplyCalled bool

	// ReplyCount tracks total replies sent this turn. If zero at the
	// end, the fallback reply kicks in.
	ReplyCount int

	// DoneCalled signals the agent is finished with this turn.
	DoneCalled bool

	// SavedMemories tracks memories saved this turn (for reflection trigger).
	SavedMemories []string

	// ReplyCost accumulates cost from chat model calls.
	ReplyCost float64

	// ReplyUsedFallback is set when the chat model falls back to a
	// secondary model.
	ReplyUsedFallback bool
	ReplyModel        string

	// ReplyText stores the final response text (after deanonymization).
	ReplyText string

	// ExpenseContext holds receipt scan results for system prompt injection.
	ExpenseContext string

	// --- Config ---

	// Cfg holds the full config (prompt file paths, memory limits, etc.).
	Cfg *config.Config

	// ConfigPath is the path to config.yaml on disk. Used by set_location
	// to persist lat/lon coordinates.
	ConfigPath string

	// ScrubVault holds PII token mappings for deanonymization.
	ScrubVault *scrub.Vault

	// SimilarityThreshold is the minimum cosine similarity for semantic
	// search matches.
	SimilarityThreshold float64

	// MaxMemoryLength is the hard character limit for a single memory.
	// 0 means use the package default (300). Configured via config.yaml.
	MaxMemoryLength int

	// PersonaFile is the path to persona.md on disk.
	PersonaFile string

	// EventBus emits typed events for the TUI. Nil-safe.
	EventBus *tui.Bus

	// AgentEventCB fires an event that wakes up the main agent after
	// background work completes. Nil when not wired (e.g., tests, main agent).
	// The memory agent's notify_agent tool calls this.
	AgentEventCB AgentEventCallback

	// ClassifierSnippet is the conversation snapshot used when running the
	// classifier. When set, the memory helpers use it instead of querying the DB
	// lazily — this matters for the memory agent, which captures the snippet
	// at goroutine launch time before subsequent turns can dirty the DB.
	// Nil in the main agent path (lazy query is fine there).
	ClassifierSnippet []memory.Message

	// PreApprovedRewrites holds classifier-suggested rewrite texts that
	// should bypass the classifier if the agent saves them verbatim.
	// This prevents the self-contradiction bug where the classifier
	// suggests text and then rejects that exact text on retry.
	// Key: the rewrite text (lowercased for case-insensitive matching).
	PreApprovedRewrites map[string]bool
}
