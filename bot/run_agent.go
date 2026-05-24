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
func (b *Bot) runAgent(fe Frontend, input AgentInput) error {
	// Prevent concurrent agent runs. Two simultaneous runs would race
	// for the same conversation state.
	if b.agentBusy.Load() {
		log.Info("agent busy, ignoring concurrent message", "msg", truncate(input.UserMessage, 60))
		return fe.SendBusy()
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
	stopTypingFn := fe.StartTyping()

	// --- Turn tracker ---
	tracker := turn.NewTracker(
		input.TriggerMsgID,
		b.eventBus,
		stopTypingFn,
		truncate(input.UserMessage, 100),
		input.ConversationID,
	)
	defer tracker.StopTyping()

	// --- Trace callbacks ---
	var traceCallback tools.TraceCallback
	var memoryTraceCallback tools.TraceCallback
	var moodTraceCallback tools.TraceCallback
	var personaTraceCallback tools.TraceCallback
	var introspectionTraceCallback tools.TraceCallback
	var traceFinalize func()

	// Traces require a tele.Context for Telegram rendering. For
	// non-Telegram frontends, traces still flow through the event bus
	// (visible in the TUI) but don't get a Telegram message.
	if b.cfg.Driver.Trace {
		if tfe, ok := fe.(*TelegramFrontend); ok {
			tr := b.makeTraceCallbacks(tfe.Context())
			traceCallback = tr.getCallback("main")
			memoryTraceCallback = tr.getCallback("memory")
			moodTraceCallback = tr.getCallback("mood")
			personaTraceCallback = tr.getCallback("persona")
			introspectionTraceCallback = tr.getCallback("introspection")
			traceFinalize = tr.finalize
		}
	}
	_ = personaTraceCallback

	// --- Reply placeholder ---
	placeholderText := "\U0001F4AD"
	if input.PlaceholderText != "" {
		placeholderText = input.PlaceholderText
	}

	if err := fe.SendPlaceholder(placeholderText, input.PlaceholderHTML); err != nil {
		tracker.StopTyping()
		log.Error("sending placeholder", "err", err)
		return fe.SendError("Sorry, I'm having trouble right now. Try again in a moment?")
	}

	// --- Build callbacks from the Frontend interface ---
	statusCallback := func(status string) error { return fe.EditStatus(status) }
	sendCallback := func(text string) error { return fe.SendReply(text) }
	sendConfirmCallback := func(text string) (int64, error) { return fe.SendConfirm(text) }
	stageResetCallback := func() error { return fe.StageReset() }
	deletePlaceholderCallback := func() error { return fe.DeletePlaceholder() }
	sendPaginatedCallback := func(text string) error { return fe.SendPaginated(text) }

	// TTS callback — only wired for Telegram frontends with voice support.
	var ttsCallback tools.TTSCallback
	if tfe, ok := fe.(*TelegramFrontend); ok {
		if b.ttsClient != nil && (input.ForceTTS || b.ttsClient.ReplyMode() == "voice") {
			ttsCallback = func(text string) {
				b.sendVoiceReply(tfe.Context(), text)
			}
		}
	}

	// Stream callback — only for frontends that support streaming.
	var streamCallback tools.StreamCallback
	var stopStream func()
	if fe.SupportsStreaming() {
		if tfe, ok := fe.(*TelegramFrontend); ok {
			streamCallback, stopStream = b.makeStreamCallback(tfe.Context(), func() *tele.Message { return tfe.Placeholder() })
		}
	}

	// --- Run the agent ---
	var introWG sync.WaitGroup

	params := b.baseRunParams()
	params.Tracker = tracker
	params.IntrospectionWG = &introWG
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

	if stopStream != nil {
		stopStream()
	}

	if err != nil {
		tracker.StopTyping()
		log.Error("agent error", "err", err)
		_ = fe.SendError("Sorry, I'm having trouble thinking right now. Try again in a moment?")
		return nil
	}

	log.Infof("  %s: %s", strings.ToLower(b.cfg.Identity.Her), truncate(result.ReplyText, 100))
	log.Info("─── reply sent ───")

	// Fire background agents (mood, introspection).
	b.launchMoodAgent(input.ConversationID, moodTraceCallback, tracker, &introWG)
	b.launchIntrospectionAgent(result, params, &introWG, introspectionTraceCallback, tracker)

	if traceFinalize != nil {
		go func() {
			time.Sleep(2 * time.Second)
			traceFinalize()
		}()
	}

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
			if b.agentEventsStopped.Load() {
				return
			}
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
