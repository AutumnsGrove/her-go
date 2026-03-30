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
	"her/skills/loader"
	"her/tui"
	"her/weather"
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

	// ChatLLM is the conversational model (e.g., Deepseek). The reply
	// tool uses this to generate the user-facing response.
	ChatLLM *llm.Client

	// VisionLLM is the vision language model (e.g., Gemini Flash).
	// The view_image tool uses this. Nil if not configured.
	VisionLLM *llm.Client

	// ClassifierLLM validates memory writes (facts, moods, receipts)
	// before they hit the DB. Nil if not configured — writes pass
	// through unchanged.
	ClassifierLLM *llm.Client

	// --- Storage & search ---

	// Store is the SQLite-backed store for facts, messages, schedules,
	// expenses, and everything else that persists.
	Store *memory.Store

	// EmbedClient generates embedding vectors for semantic search.
	EmbedClient *embed.Client

	// SkillRegistry holds discovered skills for find_skill/run_skill.
	// Nil if no skills/ directory exists.
	SkillRegistry *loader.Registry

	// TavilyClient provides web search and URL extraction. Nil if not
	// configured.
	TavilyClient *search.TavilyClient

	// WeatherClient fetches current weather from Open-Meteo. Nil if
	// no lat/lon configured.
	WeatherClient *weather.Client

	// --- Callbacks (all nil-safe) ---

	StatusCallback            StatusCallback
	SendCallback              SendCallback
	TTSCallback               TTSCallback
	TraceCallback             TraceCallback
	StageResetCallback        StageResetCallback
	DeletePlaceholderCallback DeletePlaceholderCallback
	SendConfirmCallback       SendConfirmCallback

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

	// RelevantFacts are facts semantically similar to the user's message.
	RelevantFacts []memory.Fact

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

	// SavedFacts tracks facts saved this turn (for reflection trigger).
	SavedFacts []string

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

	// PersonaFile is the path to persona.md on disk.
	PersonaFile string

	// EventBus emits typed events for the TUI. Nil-safe.
	EventBus *tui.Bus

	// --- Classifier hooks (set by agent, called by tool handlers) ---
	//
	// Storing classifier logic as function fields on Context avoids a
	// circular import: tools/ can't import agent/, but it CAN call functions
	// that agent/ injects at runtime. In Python you'd use a passed-in lambda;
	// in Go it's a function field on a struct — same idea, different syntax.
	//
	// Both fields are nil-safe: handlers check ctx.ClassifierLLM != nil
	// before calling them (ClassifierLLM being non-nil is also the signal
	// that classification is enabled).

	// ClassifyWriteFunc evaluates a proposed memory write (fact, mood, receipt)
	// for quality: is it real, useful, actually stated, not transient?
	// Returns a ClassifyVerdict — call RejectionMessageFunc if !Allowed.
	ClassifyWriteFunc func(writeType, content string, snippet []memory.Message) ClassifyVerdict

	// RejectionMessageFunc builds the agent-facing rejection string from a
	// ClassifyVerdict. The detail text comes from classifiers.yaml.
	RejectionMessageFunc func(verdict ClassifyVerdict) string
}

// ClassifyVerdict is the result of a classifier check on a proposed memory
// write. Defined here (in tools/) so fact_helpers.go and other handlers can
// reference it without importing agent (which would be circular).
type ClassifyVerdict struct {
	Allowed bool   // true = write should proceed to DB
	Type    string // verdict type: "SAVE", "FICTIONAL", "LOW_VALUE", etc.
	Reason  string // human-readable explanation from the classifier
}
