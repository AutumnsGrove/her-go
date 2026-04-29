// Package bot handles the Telegram interface — receiving messages,
// running them through the agent pipeline, and managing the UI.
package bot

import (
	"context"
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
	"her/mood"
	"her/search"
	"her/tui"
	"her/voice"

	tele "gopkg.in/telebot.v4"
)

// log is the package-level logger for the bot package.
var log = logger.WithPrefix("bot")

// Bot wraps the Telegram bot and all its dependencies.
// This is a common Go pattern: a "god struct" that holds references
// to all the services a component needs. Similar to dependency injection
// in Python/Java, but done manually (Go favors explicitness over magic).
type Bot struct {
	tb               *tele.Bot
	llm              *llm.Client          // conversational model (chat)
	driverLLM         *llm.Client          // tool-calling orchestrator
	memoryAgentLLM   *llm.Client          // post-turn memory agent — nil if not configured
	moodAgentLLM     *llm.Client          // post-turn mood agent — nil if not configured
	visionLLM        *llm.Client          // vision language model (Gemini Flash) — nil if not configured
	classifierLLM    *llm.Client          // classifier for memory writes — nil if not configured
	embedClient      *embed.Client        // local embedding model for similarity
	tavilyClient  *search.TavilyClient // web search and URL extraction
	voiceClient   *voice.Client        // local STT via parakeet-server — nil if voice disabled
	ttsClient     *voice.TTSClient     // local TTS via kokoro/mlx-audio — nil if TTS disabled
	store         *memory.Store
	cfg           *config.Config
	configPath    string // path to config.yaml — needed for /traces toggle
	systemPrompt  string
	startTime     time.Time

	// moodRunner + moodSweeper are the post-turn mood pipeline. Nil
	// when cfg.MoodAgent.Model is empty. runAgent launches a
	// goroutine that calls moodRunner.RunForConversation after each
	// reply. The sweeper runs in its own goroutine started by Start().
	moodRunner      *mood.Runner
	moodSweeper     *mood.ProposalSweeper
	moodSweeperStop context.CancelFunc // cancels the sweeper goroutine on Stop()

	// moodVocab is the loaded vocab used by both the agent and the
	// /mood wizard. Shared so the two paths can't drift.
	moodVocab *mood.Vocab

	// moodWizards tracks in-flight /mood sessions per chat id.
	// Value is *wizardState. Wizards auto-expire after 10 minutes
	// (sweeper inside handleMoodWizardCallback handles the gc).
	moodWizards sync.Map

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
func New(cfg *config.Config, configPath string, llmClient *llm.Client, driverLLM *llm.Client, memoryAgentLLM *llm.Client, moodAgentLLM *llm.Client, visionLLM *llm.Client, classifierLLM *llm.Client, embedClient *embed.Client, tavilyClient *search.TavilyClient, voiceClient *voice.Client, ttsClient *voice.TTSClient, store *memory.Store, eventBus *tui.Bus) (*Bot, error) {
	// Choose update transport based on config. In poll mode (the default),
	// the bot calls Telegram every 10 seconds asking for new messages.
	// In webhook mode, Telegram POSTs updates to us — used when a CF
	// Worker sits in front and routes traffic to the right instance.
	//
	// This is like choosing between polling a queue vs. registering an
	// HTTP handler — same data, different delivery model.
	var poller tele.Poller
	switch cfg.Telegram.Mode {
	case "webhook":
		port := cfg.Telegram.WebhookPort
		if port == 0 {
			port = 8443 // default — 8765 is taken by parakeet STT
		}
		poller = &tele.Webhook{
			Listen:      fmt.Sprintf(":%d", port),
			SecretToken: cfg.Telegram.WebhookSecret,
			// IgnoreSetWebhook prevents telebot from calling Telegram's
			// setWebhook API on startup. The CF Worker is the registered
			// webhook endpoint, not this bot — if we called setWebhook
			// with localhost:8765, Telegram would try to POST there
			// directly (and fail, because we're behind NAT).
			IgnoreSetWebhook: true,
		}
		log.Info("using webhook mode", "port", port)
	default:
		// "poll" or any unrecognized value — safe default.
		poller = &tele.LongPoller{Timeout: 10 * time.Second}
		log.Info("using long-polling mode")
	}

	settings := tele.Settings{
		Token:  cfg.Telegram.Token,
		Poller: poller,
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
		tb:             tb,
		llm:            llmClient,
		driverLLM:       driverLLM,
		memoryAgentLLM: memoryAgentLLM,
		moodAgentLLM:   moodAgentLLM,
		visionLLM:      visionLLM,
		classifierLLM:  classifierLLM,
		embedClient:    embedClient,
		tavilyClient:   tavilyClient,
		voiceClient:    voiceClient,
		ttsClient:      ttsClient,
		store:          store,
		cfg:            cfg,
		configPath:     configPath,
		systemPrompt:   string(promptBytes),
		startTime:      time.Now(),
		eventBus:       eventBus,
		agentEvents:    make(chan agent.AgentEvent, 16),
	}

	// Build the mood runner + sweeper if the mood agent is configured.
	// These wire the real production path: medium-confidence inferences
	// emit Telegram proposals via bot.sendMoodProposal; the sweeper
	// edits expired proposals in place on its own goroutine.
	if moodAgentLLM != nil {
		if err := bot.initMood(); err != nil {
			return nil, fmt.Errorf("initializing mood pipeline: %w", err)
		}
	}

	// cmd wraps a handler to log the command to the command_log table.
	// This gives us usage analytics (how often /clear is used, etc.)
	// without touching any of the individual handler functions.
	cmd := func(command string, handler func(tele.Context) error) func(tele.Context) error {
		return func(c tele.Context) error {
			chatID := c.Message().Chat.ID
			convID := bot.getConversationID(chatID)
			args := strings.TrimSpace(strings.TrimPrefix(c.Message().Text, command))
			bot.store.LogCommand(command, chatID, convID, args)
			return handler(c)
		}
	}

	// Register command handlers — each wrapped with cmd() for logging.
	tb.Handle("/help", cmd("/help", bot.handleHelp))
	tb.Handle("/clear", cmd("/clear", bot.handleClear))
	tb.Handle("/stats", cmd("/stats", bot.handleStats))
	tb.Handle("/forget", cmd("/forget", bot.handleForget))
	tb.Handle("/facts", cmd("/facts", bot.handleFacts))
	tb.Handle("/reflect", cmd("/reflect", bot.handleReflect))
	tb.Handle("/persona", cmd("/persona", bot.handlePersona))
	tb.Handle("/compact", cmd("/compact", bot.handleCompact))
	tb.Handle("/status", cmd("/status", bot.handleStatus))
	tb.Handle("/restart", cmd("/restart", bot.handleRestart))
	tb.Handle("/traces", cmd("/traces", bot.handleTraces))
	tb.Handle("/reflections", cmd("/reflections", bot.handleReflections))
	tb.Handle("/mood", cmd("/mood", bot.handleMoodCommand))
	tb.Handle("/dream", cmd("/dream", bot.handleDream))
	tb.Handle("/update", cmd("/update", bot.handleUpdate))

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

	// Register location handlers for pin drops and venue shares (v0.6).
	// Locations get saved to location_history and run through the agent
	// so Mira can respond naturally or offer nearby searches.
	tb.Handle(tele.OnLocation, bot.handleLocation)
	tb.Handle(tele.OnVenue, bot.handleVenue)

	// Register inline keyboard callback handlers (v0.6).
	// Each Action value in scheduler.Button needs a handler here.
	// See bot/callbacks.go for the implementations.
	bot.registerCallbackHandlers()

	return bot, nil
}

// Start begins receiving Telegram updates (via polling or webhook).
// This blocks forever (or until the bot is stopped), so it's typically
// the last thing called in main.go.
//
// In poll mode, we call RemoveWebhook(true) first to drop any pending
// updates from a previous session. Without this, restarting the bot
// causes a delay (10-30s) while the old long-poll connection expires
// at Telegram's end, and queued messages arrive in a burst.
//
// In webhook mode, we skip RemoveWebhook — calling it would unregister
// the CF Worker's webhook URL, and Telegram would stop sending updates
// entirely until the webhook is re-registered.
func (b *Bot) Start() {
	if b.cfg.Telegram.Mode != "webhook" {
		if err := b.tb.RemoveWebhook(true); err != nil {
			log.Warn("failed to clear pending updates", "err", err)
		}
	}

	// Start the agent event consumer before the Telegram poller.
	// This goroutine handles scheduled tasks, skill failures, and
	// (future) coding agent completions — anything that triggers an
	// agent run without a user message.
	go b.consumeAgentEvents()

	// Start the mood proposal expiry sweeper if the mood pipeline
	// is configured. No-op otherwise.
	b.startMoodSweeper()

	// Check for a pending /update confirmation. If the previous binary
	// wrote a her.update_pending flag before restarting, send the stored
	// message now. This runs synchronously before Start() so the
	// confirmation arrives before any user messages are processed.
	b.CheckUpdatePending()

	log.Info("Bot is running. Listening for messages...")
	b.tb.Start()
}

// Stop gracefully shuts down the bot.
func (b *Bot) Stop() {
	if b.moodSweeperStop != nil {
		b.moodSweeperStop() // cancels the sweeper goroutine
	}
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
// SendWithID is like SendToChat but returns the allocated message ID
// so the caller can edit it later. Used by scheduler extensions that
// want to update a message in place after first sending it.
func (b *Bot) SendWithID(chatID int64, text string) (int, error) {
	chat := &tele.Chat{ID: chatID}
	msg, err := b.tb.Send(chat, text)
	if err != nil {
		return 0, err
	}
	return msg.ID, nil
}

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

	case agent.EventInboxReady:
		log.Info("inbox-ready event", "summary", evt.Summary)

		// Direct message mode: send the text straight to chat and return.
		// The memory agent uses this for explicit user-requested work
		// ("done, cleaned up 4 memories").
		if evt.DirectMessage != "" {
			if err := b.SendToChat(b.ownerChat, evt.DirectMessage); err != nil {
				log.Error("failed to send direct inbox message", "err", err)
			}
			return
		}

		// Summary-only mode: memory housekeeping (splits, dedup, cleanup)
		// that the user didn't ask for. Log it and move on — no user-facing
		// message. Running a full agent loop here produces a jarring
		// "I reorganized your memories" message mid-conversation.
		log.Info("inbox-ready: background housekeeping complete (no user message)",
			"summary", evt.Summary)
		return

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

	params := b.baseRunParams()
	params.ScrubbedUserMessage = prompt
	params.ConversationID = conversationID
	params.StatusCallback = sendFn
	params.SendCallback = sendFn

	b.agentBusy.Store(true)
	result, err := agent.Run(params)
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
//  2. Run the shared agent pipeline (typing, placeholder, callbacks, etc.)
//
// The agent orchestrates searches, generates the response via the reply tool,
// and manages memory. The placeholder message gets edited to show status
// updates and the final response as tools execute.
func (b *Bot) handleMessage(c tele.Context) error {
	// Intercept text replies when a /mood wizard is waiting on its
	// note step. The wizard handler writes the entry + acknowledges
	// via edit; we return early so the message doesn't flow through
	// the agent pipeline.
	if b.HandleMoodWizardNote(c) {
		return nil
	}

	msg := c.Message()
	userText := msg.Text
	conversationID := b.getConversationID(msg.Chat.ID)

	log.Info("─── incoming message ───")
	log.Infof("  user: %s", truncate(userText, 100))

	// Step 1: Log the raw message to SQLite.
	msgID, err := b.store.SaveMessage("user", userText, "", conversationID)
	if err != nil {
		log.Error("saving message", "err", err)
	}

	// Step 2: PII scrub the message.
	scrubResult := b.scrubText(userText)

	// Update the saved message with the scrubbed version.
	if msgID > 0 {
		b.store.UpdateMessageScrubbed(msgID, scrubResult.Text)
		b.savePIIVaultEntries(msgID, scrubResult.Vault)
	}

	// Step 3: Run the agent pipeline — typing, placeholder, callbacks,
	// TUI events, and error handling are all handled by runAgent.
	return b.runAgent(c, AgentInput{
		UserMessage:    userText,
		ScrubbedText:   scrubResult.Text,
		ScrubVault:     scrubResult.Vault,
		ConversationID: conversationID,
		TriggerMsgID:   msgID,
	})
}
