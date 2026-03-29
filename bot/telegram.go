// Package bot handles the Telegram interface — receiving messages,
// running them through the agent pipeline, and managing the UI.
package bot

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"her/agent"
	"her/config"
	"her/embed"
	"her/llm"
	"her/logger"
	"her/memory"
	"her/ocr"
	"her/scrub"
	"her/search"
	"her/skills/loader"
	"her/tui"
	"her/voice"
	"her/weather"

	tele "gopkg.in/telebot.v4"
)

// log is the package-level logger for the bot package.
var log = logger.WithPrefix("bot")

// Bot wraps the Telegram bot and all its dependencies.
// This is a common Go pattern: a "god struct" that holds references
// to all the services a component needs. Similar to dependency injection
// in Python/Java, but done manually (Go favors explicitness over magic).
type Bot struct {
	tb            *tele.Bot
	llm           *llm.Client          // conversational model (Deepseek)
	agentLLM      *llm.Client          // tool-calling orchestrator
	visionLLM     *llm.Client          // vision language model (Gemini Flash) — nil if not configured
	embedClient   *embed.Client        // local embedding model for similarity
	tavilyClient  *search.TavilyClient // web search and URL extraction
	weatherClient *weather.Client      // Open-Meteo weather — nil if not configured
	voiceClient   *voice.Client        // local STT via parakeet-server — nil if voice disabled
	ttsClient     *voice.TTSClient     // local TTS via kokoro/mlx-audio — nil if TTS disabled
	store         *memory.Store
	cfg           *config.Config
	configPath    string // path to config.yaml — needed for /traces toggle
	systemPrompt  string
	startTime     time.Time

	// conversationIDs tracks the active conversation ID per chat.
	// When /clear is called, we rotate to a new ID so the history
	// window starts fresh.
	conversationIDs sync.Map

	// pageSessions stores active paginated views per chat.
	// When a command produces output longer than Telegram's 4096-char
	// limit, it's split into pages and stored here so the ◀/▶ inline
	// buttons can serve subsequent pages. Keyed by chat ID (int64).
	pageSessions sync.Map

	// eventBus emits structured events for the TUI. Nil-safe.
	eventBus *tui.Bus

	// ocrEnabled is true if the macos-vision-ocr binary is available.
	// When true, handlePhoto runs pre-flight OCR on every photo before
	// the agent decides what to do. The OCR is local and fast (sub-200ms).
	ocrEnabled bool

	// skillRegistry holds discovered skills for find_skill/run_skill.
	// Set via SetSkillRegistry after construction. Nil-safe — if not set,
	// the agent's skill tools return "no skills available."
	skillRegistry *loader.Registry

	// agentBusy is an atomic flag the scheduler checks to avoid firing
	// tasks while a conversation turn is in progress. Set before
	// agent.Run(), cleared after. atomic.Bool is lock-free — no mutex
	// needed for a simple "is something happening?" check. Think of it
	// like a thread-safe boolean in Python (except Python's GIL makes
	// plain bools thread-safe already — Go doesn't have a GIL).
	agentBusy atomic.Bool

	// agentEvents is the channel for system events that trigger agent
	// runs without a user message. The scheduler, skill runner, and
	// (future) coding agent all emit into this channel. The bot's
	// consumeAgentEvents goroutine reads from it and calls agent.Run.
	//
	// This is Go's CSP (Communicating Sequential Processes) pattern —
	// goroutines communicate by sending messages on channels, not by
	// sharing memory. Like Python's asyncio.Queue, but built into the
	// language.
	agentEvents chan agent.AgentEvent

	// ownerChat is the Telegram chat ID for the bot owner. Used by
	// handleAgentEvent to send replies from event-triggered agent runs.
	ownerChat int64
}

// SetSkillRegistry configures the skill registry for find_skill/run_skill.
// Call after New() and before Start(). This is a setter rather than a
// constructor param to avoid making the already-long New() signature worse.
func (b *Bot) SetSkillRegistry(reg *loader.Registry) {
	b.skillRegistry = reg
}

// SetOwnerChat sets the chat ID for event-triggered agent replies.
// The owner chat is where scheduled tasks, skill failure notifications,
// and other non-user-initiated messages get sent.
func (b *Bot) SetOwnerChat(chatID int64) {
	b.ownerChat = chatID
}

// AgentEventChannel returns a write-only channel for emitting agent events.
// The scheduler and skill runner use this to trigger agent runs without
// a user message. cmd/run.go passes this to their callbacks.
func (b *Bot) AgentEventChannel() chan<- agent.AgentEvent {
	return b.agentEvents
}

// New creates and configures a new Telegram bot.
func New(cfg *config.Config, configPath string, llmClient *llm.Client, agentLLM *llm.Client, visionLLM *llm.Client, embedClient *embed.Client, tavilyClient *search.TavilyClient, weatherClient *weather.Client, voiceClient *voice.Client, ttsClient *voice.TTSClient, store *memory.Store, eventBus *tui.Bus) (*Bot, error) {
	settings := tele.Settings{
		Token:  cfg.Telegram.Token,
		Poller: &tele.LongPoller{Timeout: 10 * time.Second},
	}

	// Retry bot creation with exponential backoff. tele.NewBot calls the
	// Telegram API to validate the token — if the network hiccups at
	// startup, a single transient failure would kill the whole process.
	// This is similar to Python's tenacity.retry, but Go prefers explicit
	// loops over decorator magic.
	var (
		tb  *tele.Bot
		err error
	)
	const maxRetries = 3
	for attempt := range maxRetries {
		tb, err = tele.NewBot(settings)
		if err == nil {
			break
		}
		if attempt < maxRetries-1 {
			backoff := time.Duration(1<<attempt) * time.Second // 1s, 2s, 4s
			log.Warn("Telegram API unreachable, retrying", "attempt", attempt+1, "backoff", backoff, "err", err)
			time.Sleep(backoff)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("creating telegram bot after %d attempts: %w", maxRetries, err)
	}

	// Load the base system prompt from prompt.md.
	promptBytes, err := os.ReadFile(cfg.Persona.PromptFile)
	if err != nil {
		return nil, fmt.Errorf("reading system prompt from %s: %w", cfg.Persona.PromptFile, err)
	}

	bot := &Bot{
		tb:            tb,
		llm:           llmClient,
		agentLLM:      agentLLM,
		visionLLM:     visionLLM,
		embedClient:   embedClient,
		tavilyClient:  tavilyClient,
		weatherClient: weatherClient,
		voiceClient:   voiceClient,
		ttsClient:     ttsClient,
		store:         store,
		cfg:           cfg,
		configPath:    configPath,
		systemPrompt:  string(promptBytes),
		startTime:     time.Now(),
		eventBus:      eventBus,
		ocrEnabled:    ocr.IsAvailable(&cfg.OCR),
		agentEvents:   make(chan agent.AgentEvent, 16),
	}

	if bot.ocrEnabled {
		log.Info("OCR enabled", "engine", "apple-vision", "binary", cfg.OCR.VisionOCRPath)
	}

	// Register command handlers.
	tb.Handle("/help", bot.handleHelp)
	tb.Handle("/clear", bot.handleClear)
	tb.Handle("/stats", bot.handleStats)
	tb.Handle("/forget", bot.handleForget)
	tb.Handle("/facts", bot.handleFacts)
	tb.Handle("/reflect", bot.handleReflect)
	tb.Handle("/persona", bot.handlePersona)
	tb.Handle("/compact", bot.handleCompact)
	tb.Handle("/status", bot.handleStatus)
	tb.Handle("/restart", bot.handleRestart)
	tb.Handle("/remind", bot.handleRemind)
	tb.Handle("/schedule", bot.handleSchedule)
	tb.Handle("/traces", bot.handleTraces)
	tb.Handle("/mood", bot.handleMood)
	tb.Handle("/reflections", bot.handleReflections)

	// Register message handler for all text messages.
	tb.Handle(tele.OnText, bot.handleMessage)

	// Register photo handler for image understanding (v0.2.5).
	// In telebot, tele.OnPhoto fires when a user sends an image.
	// Photos can optionally have a caption (text alongside the image).
	tb.Handle(tele.OnPhoto, bot.handlePhoto)

	// Register voice handler for speech-to-text (v0.3).
	// tele.OnVoice fires when a user sends a voice memo (the
	// microphone button in Telegram). Audio files sent as documents
	// use tele.OnDocument instead — we only handle voice memos here.
	tb.Handle(tele.OnVoice, bot.handleVoice)

	// Register inline keyboard callback handlers (v0.6).
	// Each Action value in scheduler.Button needs a handler here.
	// See bot/callbacks.go for the implementations.
	bot.registerCallbackHandlers()

	return bot, nil
}

// Start begins polling Telegram for messages. This blocks forever
// (or until the bot is stopped), so it's typically the last thing
// called in main.go.
//
// Before polling, we call RemoveWebhook(true) to drop any pending
// updates from a previous session. Without this, restarting the bot
// causes a delay (10-30s) while the old long-poll connection expires
// at Telegram's end, and queued messages arrive in a burst.
func (b *Bot) Start() {
	if err := b.tb.RemoveWebhook(true); err != nil {
		log.Warn("failed to clear pending updates", "err", err)
	}

	// Start the agent event consumer before the Telegram poller.
	// This goroutine handles scheduled tasks, skill failures, and
	// (future) coding agent completions — anything that triggers an
	// agent run without a user message.
	go b.consumeAgentEvents()

	log.Info("Bot is running. Listening for messages...")
	b.tb.Start()
}

// Stop gracefully shuts down the bot.
func (b *Bot) Stop() {
	b.tb.Stop()
	close(b.agentEvents) // signals consumeAgentEvents goroutine to exit
}

// IsAgentBusy returns true when the bot is mid-turn (agent.Run is executing).
// The scheduler calls this to decide whether to hold a task for the next
// tick cycle rather than firing during an active conversation.
func (b *Bot) IsAgentBusy() bool {
	return b.agentBusy.Load()
}

// chatRecipient implements tele.Recipient for sending to a specific chat ID.
// In Go, interfaces are satisfied implicitly — any type that has a
// Recipient() string method satisfies tele.Recipient. No "implements"
// keyword needed. This is like Python's duck typing but checked at
// compile time.
type chatRecipient struct {
	chatID string
}

func (r chatRecipient) Recipient() string { return r.chatID }

// SendToChat sends a text message to a specific Telegram chat.
// Used by the scheduler to deliver reminders — it doesn't have a
// tele.Context, so it calls this directly with the chat ID.
func (b *Bot) SendToChat(chatID int64, text string) error {
	_, err := b.tb.Send(
		chatRecipient{chatID: fmt.Sprintf("%d", chatID)},
		text,
		&tele.SendOptions{ParseMode: tele.ModeHTML},
	)
	return err
}

// consumeAgentEvents reads from the agent event channel and handles each
// event by triggering an agent run. This runs in its own goroutine,
// started in Start().
//
// The loop exits when the channel is closed (during Stop). Events are
// processed sequentially — if the agent is busy, the event is skipped
// with a log warning rather than queued (the buffer handles short bursts).
func (b *Bot) consumeAgentEvents() {
	for evt := range b.agentEvents {
		b.handleAgentEvent(evt)
	}
}

// handleAgentEvent triggers an agent run in response to a system event.
// This is the generalized version of the scheduler's old agentFn callback.
//
// Unlike handleMessage (which has a Telegram context, placeholder message,
// and PII scrubbing), this builds a minimal RunParams with just the
// essentials. The agent's reply gets sent as a new Telegram message to
// the owner chat.
func (b *Bot) handleAgentEvent(evt agent.AgentEvent) {
	if b.ownerChat == 0 {
		log.Warn("agent event received but no owner chat configured", "type", evt.Type)
		return
	}

	// Don't start a new agent run if one is already in progress.
	if b.agentBusy.Load() {
		log.Info("agent busy, skipping event", "type", evt.Type)
		return
	}

	// Build the prompt and conversation ID based on event type.
	var prompt, conversationID string

	switch evt.Type {
	case agent.EventSchedulerFired:
		prompt = evt.Prompt
		conversationID = "scheduled"
		log.Info("handling scheduled event", "task", evt.TaskName)

	case agent.EventSkillFailed:
		prompt = fmt.Sprintf("[system] Skill %q failed: %s. "+
			"Decide whether to notify the user, retry, or take corrective action.",
			evt.SkillName, evt.Error)
		conversationID = "skill-event"
		log.Info("handling skill failure event", "skill", evt.SkillName)

	case agent.EventCodingComplete:
		// Stub — will be implemented with delegate_coding.
		log.Info("coding complete event received (not yet implemented)",
			"skill", evt.SkillName)
		return

	case agent.EventDDLDetected:
		// A skill modified its sidecar database schema. The agent acts as
		// a sysadmin — it has context about why the skill exists and can
		// judge whether the schema change makes sense.
		prompt = fmt.Sprintf("[system] Skill %q modified its database schema:\n\n```sql\n%s\n```\n\n"+
			"This is a 4th-party (AI-generated) skill. Review this DDL change and decide:\n"+
			"- If this looks normal for what the skill does, just acknowledge it briefly.\n"+
			"- If this looks suspicious or destructive (DROP TABLE, etc.), notify the user.\n"+
			"- If the skill is repeatedly making destructive changes, recommend quarantining it.\n"+
			"Keep your response concise — this is a background system event, not a conversation.",
			evt.SkillName, evt.DDLStatement)
		conversationID = "ddl-audit"
		log.Info("handling DDL audit event", "skill", evt.SkillName, "statement", evt.DDLStatement)

	default:
		log.Warn("unknown agent event type", "type", evt.Type)
		return
	}

	// Build a sendFn that sends new messages to the owner chat.
	// Event-triggered runs don't have a placeholder to edit — they
	// just send new messages directly.
	ownerChat := b.ownerChat
	sendFn := func(text string) error {
		return b.SendToChat(ownerChat, text)
	}

	b.agentBusy.Store(true)
	result, err := agent.Run(agent.RunParams{
		AgentLLM:            b.agentLLM,
		ChatLLM:             b.llm,
		VisionLLM:           b.visionLLM,
		Store:               b.store,
		EmbedClient:         b.embedClient,
		SimilarityThreshold: b.cfg.Embed.SimilarityThreshold,
		TavilyClient:        b.tavilyClient,
		WeatherClient:       b.weatherClient,
		Cfg:                 b.cfg,
		ScrubbedUserMessage: prompt,
		ConversationID:      conversationID,
		TriggerMsgID:        0, // no trigger message for events
		StatusCallback:      sendFn,
		SendCallback:        sendFn,
		ReflectionThreshold: b.cfg.Persona.ReflectionMemoryThreshold,
		RewriteEveryN:       b.cfg.Persona.RewriteEveryNReflections,
		EventBus:            b.eventBus,
		ConfigPath:          b.configPath,
		SkillRegistry:       b.skillRegistry,
	})
	b.agentBusy.Store(false)

	if err != nil {
		log.Error("agent error from event", "type", evt.Type, "err", err)
		return
	}

	log.Info("event-triggered agent run complete",
		"type", evt.Type, "reply_len", len(result.ReplyText))
}

// handleMessage is the core pipeline. In the new agent-first architecture:
//  1. Save & scrub the message
//  2. Send a placeholder Telegram message
//  3. Run the agent SYNCHRONOUSLY — it orchestrates searches, generates
//     the response via the reply tool, and manages memory
//  4. The placeholder message gets edited to show status updates and
//     the final response as tools execute
func (b *Bot) handleMessage(c tele.Context) error {
	msg := c.Message()
	userText := msg.Text

	// Get the active conversation ID for this chat.
	conversationID := b.getConversationID(msg.Chat.ID)

	log.Info("─── incoming message ───")
	log.Infof("  user: %s", truncate(userText, 100))

	// Step 1: Log the raw message to SQLite.
	msgID, err := b.store.SaveMessage("user", userText, "", conversationID)
	if err != nil {
		log.Error("saving message", "err", err)
	}

	// Step 2: PII scrub the message.
	var scrubResult *scrub.ScrubResult
	if b.cfg.Scrub.Enabled {
		scrubResult = scrub.Scrub(userText)
		if vaultCount := len(scrubResult.Vault.Entries()); vaultCount > 0 {
			log.Info("PII scrubbed", "tokens", vaultCount)
		}
	} else {
		scrubResult = &scrub.ScrubResult{
			Text:  userText,
			Vault: scrub.NewVault(),
		}
	}

	// Update the saved message with the scrubbed version.
	if msgID > 0 {
		b.store.UpdateMessageScrubbed(msgID, scrubResult.Text)
		for _, entry := range scrubResult.Vault.Entries() {
			if err := b.store.SavePIIVaultEntry(msgID, entry.Token, entry.Original, entry.EntityType); err != nil {
				log.Error("saving PII vault entry", "err", err)
			}
		}
	}

	// Step 3: Show typing indicator while we work.
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

	// Step 4: Build the trace callback FIRST if enabled — its placeholder
	// (🧠) needs to appear ABOVE the reply placeholder in chat order.
	var traceCallback agent.TraceCallback
	if b.cfg.Agent.Trace {
		traceCallback = b.makeTraceCallback(c)
	}

	// Step 5: Send the reply placeholder message that we'll edit with
	// the final response. The thinking emoji signals to the user that
	// we're processing their message.
	placeholder, sendErr := c.Bot().Send(c.Recipient(), "\U0001F4AD")
	if sendErr != nil {
		close(stopTyping)
		log.Error("sending placeholder", "err", sendErr)
		return c.Send("Sorry, I'm having trouble right now. Try again in a moment?")
	}

	// Build the status callback — edits the placeholder with the final
	// reply text (or intermediate status updates like "searching...").
	// This closes over `placeholder` so that stageResetCallback can swap
	// it to a new message after reply sends, and statusCallback automatically
	// targets the new one.
	statusCallback := func(status string) error {
		_, err := c.Bot().Edit(placeholder, status)
		return err
	}

	// sendCallback sends a NEW message (rather than editing the placeholder).
	// Used by the reply tool for follow-up replies — e.g., after "let me
	// look that up", the actual answer comes as a separate message.
	sendCallback := func(text string) error {
		_, err := c.Bot().Send(c.Recipient(), text, &tele.SendOptions{ParseMode: tele.ModeHTML})
		return err
	}

	// sendConfirmCallback sends a message with Yes/No inline buttons and
	// returns the Telegram message ID. The agent uses this for reply_confirm
	// — the message ID keys the pending_confirmations table so the callback
	// handler can look it up when the user clicks. Same closure pattern as
	// the other callbacks — it captures `c` from the outer scope.
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
		// msg.ID is an int in telebot — we cast to int64 for the DB.
		return int64(msg.ID), nil
	}

	// stageResetCallback sends a fresh placeholder after a reply is sent.
	// Because statusCallback closes over the `placeholder` variable,
	// reassigning it here means statusCallback automatically edits the
	// new message on subsequent calls. The sent reply is left untouched.
	stageResetCallback := func() error {
		newPlaceholder, err := c.Bot().Send(c.Recipient(), "\U0001F4AD")
		if err != nil {
			return fmt.Errorf("stage reset: sending new placeholder: %w", err)
		}
		placeholder = newPlaceholder
		return nil
	}

	// deletePlaceholderCallback removes the current placeholder message.
	// Called after the agent loop exits to clean up the orphan 💭 left
	// by the last stage reset.
	deletePlaceholderCallback := func() error {
		return c.Bot().Delete(placeholder)
	}

	// Build the TTS callback — fires inside execReply so voice synthesis
	// starts immediately when text is sent, not after the whole agent loop.
	var ttsCallback agent.TTSCallback
	if b.ttsClient != nil && b.ttsClient.ReplyMode() == "voice" {
		ttsCallback = func(text string) {
			b.sendVoiceReply(c, text)
		}
	}

	// Emit TurnStartEvent for the TUI
	turnStart := time.Now()
	if b.eventBus != nil {
		b.eventBus.Emit(tui.TurnStartEvent{
			Time:           turnStart,
			TurnID:         msgID,
			UserMessage:    truncate(userText, 100),
			ConversationID: conversationID,
		})
	}

	b.agentBusy.Store(true)
	result, err := agent.Run(agent.RunParams{
		AgentLLM:                  b.agentLLM,
		ChatLLM:                   b.llm,
		VisionLLM:                 b.visionLLM,
		Store:                     b.store,
		EmbedClient:               b.embedClient,
		SimilarityThreshold:       b.cfg.Embed.SimilarityThreshold,
		TavilyClient:              b.tavilyClient,
		WeatherClient:             b.weatherClient,
		Cfg:                       b.cfg,
		ScrubbedUserMessage:       scrubResult.Text,
		ScrubVault:                scrubResult.Vault,
		ConversationID:            conversationID,
		TriggerMsgID:              msgID,
		StatusCallback:            statusCallback,
		SendCallback:              sendCallback,
		StageResetCallback:        stageResetCallback,
		DeletePlaceholderCallback: deletePlaceholderCallback,
		SendConfirmCallback:       sendConfirmCallback,
		TTSCallback:               ttsCallback,
		TraceCallback:             traceCallback,
		ReflectionThreshold:       b.cfg.Persona.ReflectionMemoryThreshold,
		RewriteEveryN:             b.cfg.Persona.RewriteEveryNReflections,
		EventBus:                  b.eventBus,
		ConfigPath:                b.configPath,
		SkillRegistry:             b.skillRegistry,
	})
	b.agentBusy.Store(false)

	close(stopTyping)

	if err != nil {
		log.Error("agent error", "err", err)
		_, _ = c.Bot().Edit(placeholder, "Sorry, I'm having trouble thinking right now. Try again in a moment?")
		return nil
	}

	log.Infof("  %s: %s", strings.ToLower(b.cfg.Identity.Her), truncate(result.ReplyText, 100))
	log.Info("─── reply sent ───")

	// Emit TurnEndEvent for the TUI — now with actual metrics from the
	// agent run. TotalCost includes both agent model calls (free) and
	// chat model calls (paid). ToolCalls and FactsSaved come from the
	// agent's accumulated counters.
	if b.eventBus != nil {
		b.eventBus.Emit(tui.TurnEndEvent{
			Time:       time.Now(),
			TurnID:     msgID,
			ElapsedMs:  time.Since(turnStart).Milliseconds(),
			TotalCost:  result.TotalCost,
			ToolCalls:  result.ToolCalls,
			FactsSaved: result.FactsSaved,
		})
	}

	return nil
}
