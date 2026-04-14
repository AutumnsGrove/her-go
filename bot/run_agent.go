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
	"time"

	"her/agent"
	"her/scrub"
	"her/tools"
	"her/tui"

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
	// refresh it every 4. The goroutine exits when we close the channel.
	// Same pattern as Python's asyncio.create_task() — fire and forget,
	// clean up via signal.
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

	// --- Trace callback ---
	// Built FIRST if enabled — its placeholder (🧠) needs to appear
	// ABOVE the reply placeholder in chat order.
	var traceCallback tools.TraceCallback
	if b.cfg.Agent.Trace {
		traceCallback = b.makeTraceCallback(c)
	}

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
		close(stopTyping)
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

	// --- TUI events ---
	turnStart := time.Now()
	if b.eventBus != nil {
		b.eventBus.Emit(tui.TurnStartEvent{
			Time:           turnStart,
			TurnID:         input.TriggerMsgID,
			UserMessage:    truncate(input.UserMessage, 100),
			ConversationID: input.ConversationID,
		})
	}

	// --- Run the agent ---
	params := b.baseRunParams()
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
	params.TraceCallback = traceCallback
	params.ImageBase64 = input.ImageBase64
	params.ImageMIME = input.ImageMIME
	params.OCRText = input.OCRText

	b.agentBusy.Store(true)
	result, err := agent.Run(params)
	b.agentBusy.Store(false)

	close(stopTyping)

	if err != nil {
		log.Error("agent error", "err", err)
		_, _ = c.Bot().Edit(placeholder, "Sorry, I'm having trouble thinking right now. Try again in a moment?")
		return nil
	}

	log.Infof("  %s: %s", strings.ToLower(b.cfg.Identity.Her), truncate(result.ReplyText, 100))
	log.Info("─── reply sent ───")

	// --- TUI end event ---
	if b.eventBus != nil {
		b.eventBus.Emit(tui.TurnEndEvent{
			Time:       time.Now(),
			TurnID:     input.TriggerMsgID,
			ElapsedMs:  time.Since(turnStart).Milliseconds(),
			TotalCost:  result.TotalCost,
			ToolCalls:  result.ToolCalls,
			FactsSaved: result.FactsSaved,
		})
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
		AgentLLM:            b.agentLLM,
		ChatLLM:             b.llm,
		VisionLLM:           b.visionLLM,
		ClassifierLLM:       b.classifierLLM,
		Store:               b.store,
		EmbedClient:         b.embedClient,
		SimilarityThreshold: b.cfg.Embed.SimilarityThreshold,
		TavilyClient:        b.tavilyClient,
		Cfg:                 b.cfg,
		ReflectionThreshold: b.cfg.Persona.ReflectionMemoryThreshold,
		RewriteEveryN:       b.cfg.Persona.RewriteEveryNReflections,
		EventBus:            b.eventBus,
		ConfigPath:          b.configPath,
	}
}
