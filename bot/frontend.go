package bot

// Frontend abstracts the I/O transport for a conversation turn.
//
// Telegram, HTTP, and future transports (Discord, web UI) all
// implement this interface. The agent pipeline calls these methods
// to communicate with the user — it never knows or cares which
// transport is underneath. Same pattern as the Store interface:
// consumers work with the contract, not the implementation.
//
// In Python terms, this is like an abstract base class (abc.ABC)
// with required methods. Go checks at compile time instead of
// runtime, so you get the error immediately if a method is missing.
type Frontend interface {
	// SendPlaceholder sends an initial "thinking" indicator and returns
	// an opaque handle. Telegram shows 💭 as an editable message;
	// HTTP mode may return a no-op handle.
	SendPlaceholder(text string, html bool) error

	// EditStatus updates the placeholder with a status message or the
	// final reply text. On Telegram this edits the placeholder message;
	// on HTTP this buffers the text for the response.
	EditStatus(text string) error

	// SendReply sends a NEW message (separate from the placeholder).
	// Used for follow-up messages, not edits.
	SendReply(text string) error

	// SendPaginated sends a long message that may need pagination.
	// Telegram splits it into pages with ◀/▶ buttons; HTTP sends the
	// full text.
	SendPaginated(text string) error

	// SendConfirm sends a confirmation prompt and returns a message ID.
	// Telegram shows inline Yes/No buttons; HTTP may skip this.
	SendConfirm(text string) (msgID int64, err error)

	// StageReset sends a fresh placeholder after a reply tool fires,
	// so the next status update targets a new message. On HTTP this
	// may be a no-op.
	StageReset() error

	// DeletePlaceholder removes the orphan placeholder left by the
	// last stage reset.
	DeletePlaceholder() error

	// StartTyping begins a typing indicator and returns a function
	// that stops it. On Telegram this sends "typing..." every 4s.
	// On HTTP this is a no-op.
	StartTyping() (stop func())

	// SupportsStreaming returns true if this frontend can handle
	// incremental token delivery (live typing effect).
	SupportsStreaming() bool

	// OnStreamToken receives a single token during streaming.
	// Only called when SupportsStreaming() returns true.
	OnStreamToken(token string)

	// StopStream signals that streaming is complete.
	StopStream()

	// SendBusy tells the user the agent is already processing another
	// message. Returns an error so the caller can propagate it.
	SendBusy() error

	// SendError tells the user something went wrong.
	SendError(text string) error

	// ReplyText returns the accumulated reply text. Used by HTTP
	// frontend to build the JSON response. Telegram returns empty
	// (replies are sent inline).
	ReplyText() string
}

// TraceProvider is an optional interface that a Frontend can implement
// to receive agent trace callbacks. When runAgent detects a Frontend
// that satisfies TraceProvider, it wires the trace callbacks through
// it — no Telegram type assertion needed.
//
// This is how the gateway bridges traces to adapters: the
// gatewayFrontend implements TraceProvider and routes trace text
// to the adapter's OnTraceEvent method.
type TraceProvider interface {
	// TraceCallback returns a trace callback for the named slot
	// ("main", "memory", "mood", "persona", "introspection").
	TraceCallback(slot string) func(text string) error

	// TraceFinalize is called after the turn completes to do any
	// cleanup (e.g., store the final snapshot).
	TraceFinalize()
}
