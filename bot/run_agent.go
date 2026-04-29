// run_agent.go — Shared agent pipeline for all message handlers.
//
// Before this file existed, every handler (text, photo, voice, location)
// copied ~80 lines of identical boilerplate: typing indicator, placeholder
// message, callback wiring, agent.Run, error handling, TUI events. This
// file extracts that into two reusable pieces:
//
//   - runAgent: the full interactive pipeline (typing, placeholder, traces,
//     all callbacks, TUI events). Used by user-facing message handlers.
//   - baseRunParams: pre-fills the 15+ constant fields from Bot. Used by
//     both runAgent and the simpler event/callback handlers that don't need
//     the full UI machinery.
package bot

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"her/agent"
	"her/scrub"
	"her/tools"
	"her/turn"

	tele "gopkg.in/telebot.v4"
)

// AgentInput holds the variable parts of an interactive agent run.
// Everything constant (LLM clients, store, config, etc.) comes from the Bot
// struct automatically. Think of this as the "what's different about THIS
// particular message" — the rest is infrastructure.
//
// In Python terms, this is like a dataclass for keyword arguments:
//
//	@dataclass
//	class AgentInput:
//	    user_message: str
//	    conversation_id: str
//	    ...
type AgentInput struct {
	// UserMessage is the original user text (or synthetic prompt for
	// locations/events). Used in logs and TUI events.
	UserMessage string

	// ScrubbedText is the PII-scrubbed version sent to the agent.
	// If empty, UserMessage is used as-is (no scrubbing needed).
	ScrubbedText string

	// ScrubVault holds PII tokens for deanonymization in replies.
	// Nil means an empty vault is created automatically.
	ScrubVault *scrub.Vault

	ConversationID string
	TriggerMsgID   int64

	// Media fields — only populated by the photo handler.
	ImageBase64 string
	ImageMIME   string
	OCRText     string

	// PlaceholderText overrides the default 💭 placeholder message.
	// The voice handler uses this to show the transcript while thinking.
	PlaceholderText string

	// PlaceholderHTML enables HTML parse mode on the placeholder and
	// subsequent status edits. Needed when PlaceholderText contains
	// HTML formatting (e.g., the voice handler's <i>transcript</i>).
	PlaceholderHTML bool

	// ForceTTS enables TTS regardless of the configured reply mode.
	// The voice handler sets this so voice memos always get voice replies,
	// even when reply_mode isn't "voice".
	ForceTTS bool
}

// runAgent runs the full interactive agent pipeline: typing indicator,
// placeholder message, trace callback, all Telegram callbacks, TUI events,
// and error handling. This is what every user-facing message handler calls
// after doing its input-specific prep work (downloading photos, transcribing
// voice, scrubbing PII, etc.).
//
// The flow:
//  1. Start typing indicator (refreshed every 4s)
//  2. Send trace placeholder (if enabled) — appears above the reply
//  3. Send reply placeholder (💭)
//  4. Build all callbacks (status, send, stage reset, confirm, TTS)
//  5. Emit TurnStartEvent for the TUI
//  6. Run agent.Run synchronously
//  7. Clean up (stop typing, emit TurnEndEvent)
//  8. Handle errors (edit placeholder with error message)
func (b *Bot) runAgent(c tele.Context, input AgentInput) error {
	// Prevent concurrent agent runs. Telegram can deliver the same update twice
	// (retry on slow response) or the user can send two messages before the
	// first turn finishes. Either way, only one agent turn runs at a time.
	// The message is not dropped — we tell the user and let them resend.
	if b.agentBusy.Load() {
		log.Info("agent busy, ignoring concurrent message", "msg", truncate(input.UserMessage, 60))
		return c.Send("Still working on your last message — give me just a moment.")
	}

	// Apply defaults for optional fields.
	scrubbedText := input.ScrubbedText
	if scrubbedText == "" {
		scrubbedText = input.UserMessage
	}
	vault := input.ScrubVault
	if vault == nil {
		vault = scrub.NewVault()
	}

	// --- Typing indicator ---
	// Telegram's typing indicator expires after ~5 seconds, so we
	// refresh it every 4. The Tracker wraps the stop function in
	// sync.Once, so it's safe to call from the reply tool (early stop),
	// from the Tracker when all phases finish, or from the defer below
	// (panic safety). No more leaked goroutines.
	stopTypingFn := b.startTypingIndicator(c)

	// --- Turn tracker ---
	// The Tracker manages the full lifecycle of this message turn,
	// including background agents. It emits TurnStartEvent now and
	// TurnEndEvent when all phases (main, memory, mood) complete.
	tracker := turn.NewTracker(
		input.TriggerMsgID,
		b.eventBus,
		stopTypingFn,
		truncate(input.UserMessage, 100),
		input.ConversationID,
	)
	defer tracker.StopTyping() // panic safety — guarantees typing stops

	// --- Trace callbacks ---
	// Trace callbacks pull from a single per-turn trace.Board. Each
	// registered stream gets its own slot; render order comes from
	// the trace.Streams() registry so main always shows before
	// memory / mood. New agents in new packages can request a
	// callback just by registering + calling getTrace("their-slot").
	var traceCallback tools.TraceCallback
	var memoryTraceCallback tools.TraceCallback
	var moodTraceCallback tools.TraceCallback
	var personaTraceCallback tools.TraceCallback
	var traceFinalize func()
	if b.cfg.Driver.Trace {
		tr := b.makeTraceCallbacks(c)
		traceCallback = tr.getCallback("main")
		memoryTraceCallback = tr.getCallback("memory")
		moodTraceCallback = tr.getCallback("mood")
		personaTraceCallback = tr.getCallback("persona")
		traceFinalize = tr.finalize
	}
	// Suppress unused-variable warning — personaTraceCallback is wired
	// up for future use by the dreamer/reflect path.
	_ = personaTraceCallback

	// --- Reply placeholder ---
	// The thinking emoji (💭) signals to the user that we're processing.
	// Voice handler overrides this with the transcript text.
	placeholderText := "\U0001F4AD"
	if input.PlaceholderText != "" {
		placeholderText = input.PlaceholderText
	}

	// Build send options — HTML parse mode only when the placeholder
	// contains HTML (e.g., voice handler's <i>transcript</i>).
	var placeholderOpts []interface{}
	if input.PlaceholderHTML {
		placeholderOpts = append(placeholderOpts, &tele.SendOptions{ParseMode: tele.ModeHTML})
	}

	placeholder, sendErr := c.Bot().Send(c.Recipient(), placeholderText, placeholderOpts...)
	if sendErr != nil {
		tracker.StopTyping()
		log.Error("sending placeholder", "err", sendErr)
		return c.Send("Sorry, I'm having trouble right now. Try again in a moment?")
	}

	// --- Callbacks ---
	// These closures all capture `placeholder` by reference. When
	// stageResetCallback reassigns it, statusCallback automatically
	// targets the new message. This is the same closure behavior as
	// Python — variables are looked up at call time, not definition time.

	// statusCallback edits the placeholder with status updates or the
	// final reply text.
	statusCallback := func(status string) error {
		if input.PlaceholderHTML {
			_, err := c.Bot().Edit(placeholder, status, &tele.SendOptions{ParseMode: tele.ModeHTML})
			return err
		}
		_, err := c.Bot().Edit(placeholder, status)
		return err
	}

	// sendCallback sends a NEW message (for follow-up replies, not edits).
	sendCallback := func(text string) error {
		_, err := c.Bot().Send(c.Recipient(), text, &tele.SendOptions{ParseMode: tele.ModeHTML})
		return err
	}

	// sendConfirmCallback sends a message with Yes/No inline buttons
	// and returns the Telegram message ID for the pending_confirmations table.
	sendConfirmCallback := func(text string) (int64, error) {
		markup := &tele.ReplyMarkup{}
		btnYes := markup.Data("Yes", "confirm", "yes")
		btnNo := markup.Data("No", "confirm", "no")
		markup.Inline(markup.Row(btnYes, btnNo))

		msg, err := c.Bot().Send(c.Recipient(), text, &tele.SendOptions{
			ParseMode:   tele.ModeHTML,
			ReplyMarkup: markup,
		})
		if err != nil {
			return 0, err
		}
		return int64(msg.ID), nil
	}

	// stageResetCallback sends a fresh placeholder after a reply is sent.
	// Because statusCallback closes over the `placeholder` variable,
	// reassigning it here means statusCallback automatically edits the
	// new message on subsequent calls.
	stageResetCallback := func() error {
		newPlaceholder, err := c.Bot().Send(c.Recipient(), "\U0001F4AD")
		if err != nil {
			return fmt.Errorf("stage reset: sending new placeholder: %w", err)
		}
		placeholder = newPlaceholder
		return nil
	}

	// deletePlaceholderCallback removes the orphan 💭 left by the
	// last stage reset after the agent loop exits.
	deletePlaceholderCallback := func() error {
		return c.Bot().Delete(placeholder)
	}

	// TTS callback — fires inside execReply so voice synthesis starts
	// immediately when text is sent, not after the whole agent loop.
	// ForceTTS (set by voice handler) bypasses the reply mode check.
	var ttsCallback tools.TTSCallback
	if b.ttsClient != nil && (input.ForceTTS || b.ttsClient.ReplyMode() == "voice") {
		ttsCallback = func(text string) {
			b.sendVoiceReply(c, text)
		}
	}

	// Stream callback — delivers live tokens to Telegram as the chat model
	// generates them, creating a typing effect. Only active when streaming
	// is enabled in config. getPlaceholder is a closure so it always returns
	// the current placeholder, even after a stageResetCallback reassignment.
	var streamCallback tools.StreamCallback
	var stopStream func()
	if b.cfg.Chat.Streaming {
		streamCallback, stopStream = b.makeStreamCallback(c, func() *tele.Message { return placeholder })
	}

	// sendPaginatedCallback wraps the bot's pagination system for the
	// reply tool. When a message exceeds Telegram's 4096-char limit,
	// this splits it into pages with ◀/▶ navigation buttons. The pages
	// are stored in Bot.pageSessions and served by handlePageCallback.
	sendPaginatedCallback := func(text string) error {
		return b.sendPaginated(c, text)
	}

	// --- Run the agent ---
	params := b.baseRunParams()
	params.Tracker = tracker
	params.ScrubbedUserMessage = scrubbedText
	params.ScrubVault = vault
	params.ConversationID = input.ConversationID
	params.TriggerMsgID = input.TriggerMsgID
	params.StatusCallback = statusCallback
	params.SendCallback = sendCallback
	params.StageResetCallback = stageResetCallback
	params.DeletePlaceholderCallback = deletePlaceholderCallback
	params.SendConfirmCallback = sendConfirmCallback
	params.TTSCallback = ttsCallback
	params.StreamCallback = streamCallback
	params.SendPaginatedCallback = sendPaginatedCallback
	params.TraceCallback = traceCallback
	params.MemoryTraceCallback = memoryTraceCallback
	params.ImageBase64 = input.ImageBase64
	params.ImageMIME = input.ImageMIME
	params.OCRText = input.OCRText

	b.agentBusy.Store(true)
	result, err := agent.Run(params)
	b.agentBusy.Store(false)

	// Stop the stream ticker before stopping typing — both may try to
	// edit the placeholder, and we want the authoritative StatusCallback
	// edit (deanonymized text, already sent inside agent.Run) to be last.
	if stopStream != nil {
		stopStream()
	}

	if err != nil {
		tracker.StopTyping()
		log.Error("agent error", "err", err)
		_, _ = c.Bot().Edit(placeholder, "Sorry, I'm having trouble thinking right now. Try again in a moment?")
		return nil
	}

	log.Infof("  %s: %s", strings.ToLower(b.cfg.Identity.Her), truncate(result.ReplyText, 100))
	log.Info("─── reply sent ───")

	// Fire the mood agent in a goroutine. Begin the phase BEFORE
	// launching the goroutine to prevent a race where all known phases
	// finish and TurnEndEvent fires before the mood goroutine starts.
	b.launchMoodAgent(input.ConversationID, moodTraceCallback, tracker)

	// Finalize the trace — store the snapshot for /lasttrace and
	// paginate overflow if the trace exceeds Telegram's char limit.
	// This runs after the main agent completes but before mood/memory
	// background agents finish, so their slots may still be updating.
	// That's OK — /lasttrace can be called later to get the full picture.
	if traceFinalize != nil {
		// Small delay lets the background agents write their initial
		// content before we snapshot. Not critical — the snapshot
		// captures whatever's in the board at this moment.
		go func() {
			time.Sleep(2 * time.Second)
			traceFinalize()
		}()
	}

	// No manual TurnEndEvent here — the Tracker emits it when all
	// phases (main, memory, mood) complete, with accumulated metrics.

	return nil
}

// baseRunParams returns a RunParams pre-filled with all the constant fields
// from the Bot struct. The 15+ fields that are identical across every agent
// run (LLM clients, store, config, thresholds, etc.) are set once here.
//
// Callers set the variable fields (message, vault, conversation ID, callbacks)
// before passing to agent.Run. Used by both runAgent (for the full interactive
// pipeline) and simpler handlers like handleAgentEvent that don't need the
// full UI machinery.
func (b *Bot) baseRunParams() agent.RunParams {
	return agent.RunParams{
		DriverLLM:           b.driverLLM,
		MemoryAgentLLM:      b.memoryAgentLLM,
		ChatLLM:             b.llm,
		VisionLLM:           b.visionLLM,
		ClassifierLLM:       b.classifierLLM,
		Store:               b.store,
		EmbedClient:         b.embedClient,
		SimilarityThreshold: b.cfg.Embed.SimilarityThreshold,
		TavilyClient:        b.tavilyClient,
		Cfg:                 b.cfg,
		EventBus:            b.eventBus,
		ConfigPath:          b.configPath,
		// Wire the agent event callback so the memory agent's notify_agent
		// tool can wake up the driver agent for a follow-up message.
		// Non-blocking send: if the channel is full (16 buffered events),
		// drop the event rather than deadlocking the memory agent goroutine.
		AgentEventCB: func(summary, directMessage string) {
			evt := agent.AgentEvent{
				Type:          agent.EventInboxReady,
				Summary:       summary,
				DirectMessage: directMessage,
				Timestamp:     time.Now(),
			}
			select {
			case b.agentEvents <- evt:
			default:
				log.Warn("agent event channel full, dropping inbox-ready event",
					"summary", summary)
			}
		},
	}
}

// startTypingIndicator launches the Telegram typing indicator goroutine
// and returns a function that stops it. The returned function closes a
// channel — it's safe to call via sync.Once (which the Tracker does).
//
// Extracted so runAgent doesn't own the channel directly — the Tracker
// wraps this in sync.Once for panic-safe cleanup.
func (b *Bot) startTypingIndicator(c tele.Context) func() {
	stopTyping := make(chan struct{})
	go func() {
		_ = c.Notify(tele.Typing)
		ticker := time.NewTicker(4 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-stopTyping:
				return
			case <-ticker.C:
				_ = c.Notify(tele.Typing)
			}
		}
	}()
	return func() { close(stopTyping) }
}

// makeStreamCallback creates a streaming callback that funnels LLM tokens to
// Telegram via incremental message edits. Returns the callback (for injection
// into tools.Context.StreamCallback) and a stop function that shuts down the
// background ticker goroutine.
//
// Design: tokens arrive fast (one per ~10ms) but Telegram's edit rate limit
// is ~1 per second per message. We collect tokens in a mutex-protected buffer
// and flush to Telegram every 400ms — fast enough to feel live, safely under
// the rate limit. A "▋" block cursor appends while streaming to signal the
// response is still in progress.
//
// getPlaceholder is a closure so that stageResetCallback's reassignment of
// the placeholder variable is picked up at flush time rather than capturing
// the pointer value at construction time.
func (b *Bot) makeStreamCallback(c tele.Context, getPlaceholder func() *tele.Message) (tools.StreamCallback, func()) {
	var mu sync.Mutex
	var buf strings.Builder
	var lastFlushed string
	done := make(chan struct{})

	// Flush goroutine — edits the Telegram message every 400ms.
	go func() {
		ticker := time.NewTicker(400 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				mu.Lock()
				current := buf.String()
				mu.Unlock()
				if current == lastFlushed || current == "" {
					continue
				}
				// Append cursor so the user can see the reply is still coming.
				_, err := c.Bot().Edit(getPlaceholder(), current+"▋")
				if err != nil && !strings.Contains(err.Error(), "not modified") {
					// Rate limit or edit conflict — skip this tick, try next.
					continue
				}
				lastFlushed = current
			}
		}
	}()

	cb := func(chunk string) error {
		mu.Lock()
		buf.WriteString(chunk)
		mu.Unlock()
		return nil
	}

	stop := func() {
		close(done)
	}

	return cb, stop
}
