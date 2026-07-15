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
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"her/calendar"
	"her/config"
	"her/gmail"
	"her/embed"
	"her/llm"
	"her/memory"
	"her/scrub"
	"her/search"
	"her/tui"
	"her/turn"
)

// ---------------------------------------------------------------------------
// Telegram constants
// ---------------------------------------------------------------------------

// TelegramMaxMessageLen is Telegram's hard limit for message length in characters.
// Messages exceeding this limit are rejected with MESSAGE_TOO_LONG.
// Reference: https://core.telegram.org/bots/api#sendmessage
const TelegramMaxMessageLen = 4096

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
// without a user message. Used by notify_agent to wake up the driver agent
// after the memory agent finishes background work.
//
// The callback is defined here (not in agent/) to avoid circular imports.
// The bot layer provides the implementation that translates these params
// into an agent.AgentEvent and writes it to the agent event channel.
type AgentEventCallback func(summary, directMessage string)

// MessageSendCallback fires when a reply is delivered to the user. The reply
// tool calls this after confirmed delivery — it replaces the old TTSCallback
// with a richer context that allows multiple post-delivery actions (TTS,
// analytics, message logging, etc.) via a single hook point.
//
// Defined here (not in agent_engine/) to avoid circular imports between
// tools/ and agent_engine/. The bot layer wires this from TurnHooks.
type MessageSendCallback func(info MessageSendInfo)

// MessageSendInfo carries the details of a delivered reply.
type MessageSendInfo struct {
	Text           string  // the reply text
	IsFirstReply   bool    // true for the first reply in this turn (edits placeholder)
	IsContinuation bool    // true for follow-up replies after the first
	Model          string  // which model generated this reply
	UsedFallback   bool    // true if the chat model fell back
	CostUSD        float64 // cost of the chat completion
}

// SendPaginatedCallback sends a message split into pages with ◀/▶ navigation
// buttons when it exceeds Telegram's 4096-char limit. The reply tool calls
// this when the combined response (chat text + place cards) is too long for
// a single message. The bot layer handles page storage and button callbacks.
type SendPaginatedCallback func(text string) error

// ---------------------------------------------------------------------------
// PlaceCard — a pre-formatted place result ready for deterministic rendering.
//
// Built by nearby_search from Foursquare (or Tavily fallback) results.
// The reply tool appends these as a block after the chat model's response,
// so the user gets reliable formatting (address, distance, Maps link)
// without relying on the LLM to reproduce structured data accurately.
// ---------------------------------------------------------------------------

// PlaceCard holds everything needed to render a single place result.
// All fields are pre-computed by the nearby_search handler — no
// downstream code needs to do math or URL construction.
type PlaceCard struct {
	Name         string  // "Blue Bottle Coffee"
	Category     string  // "Coffee Shop" (joined if multiple)
	DistanceText string  // "350m away" or "1.2km (~15 min walk)"
	Address      string  // "123 Main St, Portland, OR"
	MapsURL      string  // "https://maps.google.com/?q=45.523,-122.676"
	Lat          float64 // raw coordinates (for future use)
	Lon          float64
}

// FormatPlaceCards renders a slice of PlaceCards into a Telegram-ready
// block that gets appended after the chat model's response. Uses a
// simple text format with emoji markers — deterministic, no LLM needed.
//
// Returns empty string if there are no cards (so the reply tool can
// skip appending without an extra check).
func FormatPlaceCards(cards []PlaceCard) string {
	if len(cards) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n\n───\n")
	for _, c := range cards {
		// Name + category.
		b.WriteString("📍 ")
		b.WriteString(c.Name)
		if c.Category != "" {
			b.WriteString(" (")
			b.WriteString(c.Category)
			b.WriteString(")")
		}
		// Distance.
		if c.DistanceText != "" {
			b.WriteString(" — ")
			b.WriteString(c.DistanceText)
		}
		b.WriteString("\n")

		// Address on its own line, indented.
		if c.Address != "" {
			b.WriteString("   ")
			b.WriteString(c.Address)
			b.WriteString("\n")
		}

		// Maps link on its own line, indented.
		if c.MapsURL != "" {
			b.WriteString("   → ")
			b.WriteString(c.MapsURL)
			b.WriteString("\n")
		}
	}

	return b.String()
}

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
	// AgentName identifies which agent is running ("main", "memory",
	// "introspection", "dream"). Used by tools like use_tools to scope
	// deferred loading to the agent's declared tool set.
	AgentName string

	// IsSimRun is true when running through the sim adapter. Used by
	// reply_direct to enforce the sim-only guard.
	IsSimRun bool

	// DreamRemoveCount tracks how many memories the dream agent has removed
	// this cycle. Used by the forgetting guard to enforce MaxRemovesPerCycle.
	DreamRemoveCount int

	// Ctx is the parent context for this turn. Tools that spawn retries
	// or background work should derive from this so they respect shutdown
	// and turn cancellation. Defaults to context.Background() if unset.
	Ctx context.Context

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
	Store memory.Store

	// EmbedClient generates embedding vectors for semantic search.
	EmbedClient *embed.Client

	// SearXNGClient provides web search via a self-hosted SearXNG instance.
	// Optional — if set, web_search uses this instead of Tavily. Nil if
	// not configured.
	SearXNGClient *search.SearXNGClient

	// TavilyClient provides web search and URL extraction. Nil if not
	// configured.
	TavilyClient *search.TavilyClient

	// CalendarBridge provides calendar operations. In production this is
	// a CLIBridge (shells out to Swift EventKit). In sims/tests it's a
	// FakeBridge (in-memory). Nil means calendar tools fall back to creating
	// a CLIBridge on demand (backward-compatible with existing code).
	CalendarBridge calendar.Bridge

	// GmailBridge provides read-only email access. In production this is
	// an APIBridge (Gmail API via OAuth2). In sims it's a FakeBridge
	// (in-memory, seeded from YAML). Nil means email tools return a
	// "not configured" error.
	GmailBridge gmail.Bridge

	// --- Callbacks (all nil-safe) ---

	StatusCallback            StatusCallback
	SendCallback              SendCallback
	TTSCallback               TTSCallback // DEPRECATED: use OnMessageSend instead. Kept for backward compat during migration.
	TraceCallback             TraceCallback
	StageResetCallback        StageResetCallback
	DeletePlaceholderCallback DeletePlaceholderCallback
	SendConfirmCallback       SendConfirmCallback
	// StreamCallback delivers streaming tokens to Telegram in real time.
	// Nil means streaming is disabled for this turn — reply falls back to
	// the existing non-streaming path automatically.
	StreamCallback StreamCallback

	// OnMessageSend fires after a reply is delivered to the user. Replaces
	// TTSCallback with a richer hook that carries model info and cost. The
	// bot layer wires this from the TurnHooks registry.
	OnMessageSend MessageSendCallback

	// SendPaginatedCallback sends a message split into pages with inline
	// navigation buttons. Used by the reply tool when the combined response
	// (chat text + place cards) exceeds Telegram's 4096-char limit. Nil
	// means pagination isn't supported for this turn — the reply tool will
	// fall back to rejecting long messages.
	SendPaginatedCallback SendPaginatedCallback

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

	// ScheduleID is the scheduler_tasks.id that triggered this agent run
	// (0 if not from a schedule). Used to tag messages as schedule-triggered
	// so the schedule_context layer can enable "delete this reminder" UX.
	ScheduleID int64

	// ConversationSummary is the compacted summary of older messages.
	// Injected into the system prompt for context.
	ConversationSummary string

	// AgentPassedMemories holds memories the agent explicitly chose via
	// recall_memories and passed to reply(memories=[...]). These are the
	// ONLY memories the chat model sees — there is no auto-injection fallback.
	AgentPassedMemories []string

	// SearchContext accumulates search results, book data, and URL
	// content across tool calls. Included in the reply prompt.
	SearchContext string

	// PlaceCards holds pre-formatted place cards from nearby_search.
	// The reply tool appends these as a structured block after the chat
	// model's response — deterministic formatting that the LLM can't
	// mangle. Each card includes name, category, distance, address,
	// and a clickable Google Maps link, all pre-computed.
	PlaceCards []PlaceCard

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

	// ReportsDir is the absolute path to the reports/ directory where
	// the worker agent writes file artifacts (briefings, research, etc.).
	// File tools (write_file, read_file, etc.) enforce this as a security
	// boundary — paths that escape this directory are rejected.
	ReportsDir string

	// WorkerCallback fires the worker agent in a background goroutine.
	// Called by send_task when target="worker" and wait=false. Nil-safe —
	// returns an error message when the worker agent is not configured.
	// triggerMsgID links the worker's agent_turns to the parent message.
	WorkerCallback func(taskType, note string, triggerMsgID int64)

	// WorkerCallbackSync runs the worker agent synchronously and returns
	// its result summary. Called by send_task when wait=true — the driver
	// agent blocks until the worker finishes, then gets the summary as
	// a tool response inline. Nil-safe.
	WorkerCallbackSync func(taskType, note string, triggerMsgID int64) string

	// PendingNarration stores cleaned report text that should be sent
	// as a voice memo after the turn completes. Set by narrate_report,
	// consumed by the bot layer after the reply's own TTS finishes.
	PendingNarration string

	// PublishedReportURL stores a Telegraph URL from publish_report.
	// The bot layer auto-appends this as a clickable link after the
	// reply — guarantees the URL appears even if the LLM forgets it.
	PublishedReportURL string

	// EventBus emits typed events for the TUI. Nil-safe.
	EventBus *tui.Bus

	// AgentEventCB fires an event that wakes up the driver agent after
	// background work completes. Nil when not wired (e.g., tests, driver agent).
	// The memory agent's notify_agent tool calls this.
	AgentEventCB AgentEventCallback

	// ClassifierSnippet is the conversation snapshot used when running the
	// classifier. When set, the memory helpers use it instead of querying the DB
	// lazily — this matters for the memory agent, which captures the snippet
	// at goroutine launch time before subsequent turns can dirty the DB.
	// Nil in the driver agent path (lazy query is fine there).
	ClassifierSnippet []memory.Message

	// SelfOnly restricts memory tools (recall_memories, list_cards) to
	// self-subject data only. Set by the introspection agent so it only
	// sees self-memories and self-cards, not user facts.
	SelfOnly bool

	// ThinkTraces accumulates think() call contents during the driver
	// agent loop. Used by the auto-inject chat layer to build a semantic
	// query for self-memory search.
	ThinkTraces []string

	// WorkerSummary stores the text from the summary tool. The worker
	// agent calls summary(text="...") to record its findings before
	// calling done. RunWorker reads this after the loop.
	WorkerSummary string

	// WrittenFiles tracks files created by write_file during this run.
	// RunWorker reads this to determine which report file to attach,
	// instead of guessing from filesystem timestamps.
	WrittenFiles []string

	// PreApprovedRewrites holds classifier-suggested rewrite texts that
	// should bypass the classifier if the agent saves them verbatim.
	// This prevents the self-contradiction bug where the classifier
	// suggests text and then rejects that exact text on retry.
	// Key: the rewrite text (lowercased for case-insensitive matching).
	PreApprovedRewrites map[string]bool
}

// ValidateReportPath resolves a relative path against the reports directory
// and verifies it doesn't escape the boundary. Returns the absolute path
// on success. Used by all file tools (write_file, read_file, patch_file)
// to prevent path traversal attacks from prompt-injected content.
func ValidateReportPath(reportsDir, relPath string) (string, error) {
	if reportsDir == "" {
		return "", fmt.Errorf("reports directory not configured")
	}
	if filepath.IsAbs(relPath) {
		return "", fmt.Errorf("path escapes reports directory: %s", relPath)
	}
	abs := filepath.Clean(filepath.Join(reportsDir, relPath))
	if !strings.HasPrefix(abs, filepath.Clean(reportsDir)+string(filepath.Separator)) &&
		abs != filepath.Clean(reportsDir) {
		return "", fmt.Errorf("path escapes reports directory: %s", relPath)
	}
	return abs, nil
}
