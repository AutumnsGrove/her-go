// Package bot handles the Telegram interface — receiving messages,
// running them through the agent pipeline, and managing the UI.
package bot

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"her/agent"
	"her/calendar"
	"her/config"
	"her/gmail"
	"her/embed"
	"her/llm"
	"her/logger"
	"her/memory"
	"her/mood"
	"her/retry"
	"her/scrub"
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
	driverLLM        *llm.Client          // tool-calling orchestrator
	memoryAgentLLM   *llm.Client          // post-turn memory agent — nil if not configured
	moodAgentLLM     *llm.Client          // post-turn mood agent — nil if not configured
	visionLLM        *llm.Client          // vision language model (Gemini Flash) — nil if not configured
	classifierLLM    *llm.Client          // classifier for memory writes — nil if not configured
	dreamAgentLLM    *llm.Client          // memory dreamer — nil falls back to memoryAgentLLM
	introspectionLLM *llm.Client          // self-reflection agent — nil falls back to memoryAgentLLM
	embedClient      *embed.Client        // local embedding model for similarity
	tavilyClient     *search.TavilyClient // web search and URL extraction
	calendarBridge   calendar.Bridge      // nil in prod (tools create CLIBridge), FakeBridge in sims
	voiceClient      *voice.Client        // STT client (local parakeet or remote whisper) — nil if voice disabled
	ttsClient        *voice.TTSClient     // local TTS via kokoro/mlx-audio — nil if TTS disabled
	store            memory.Store
	cfg              *config.Config
	configPath       string // path to config.yaml — needed for /traces toggle
	systemPrompt     string
	startTime        time.Time
	isSimRun         bool   // true when running via the sim adapter

	// moodRunner + moodSweeper are the post-turn mood pipeline. Nil
	// when cfg.MoodAgent.Model is empty. runAgent launches a
	// goroutine that calls moodRunner.RunForConversation after each
	// reply. The sweeper runs in its own goroutine started by Start().
	moodRunner      *mood.Runner
	moodSweeper     *mood.ProposalSweeper
	moodSweeperStop context.CancelFunc // cancels the sweeper goroutine on Stop()
	shutdownCh      chan struct{}      // closed on Stop(); goroutines select on this to exit cleanly
	stopOnce        sync.Once          // ensures Stop() only runs once (prevents double-close panic)

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
	conversationIDs   sync.Map
	conversationIDsMu sync.Mutex // serialises the load-or-create path

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
	agentEvents        chan agent.AgentEvent
	agentEventsStopped atomic.Bool

	// lastTraceSnapshot stores the full Board snapshot from the most
	// recent completed turn. /lasttrace re-sends this via sendPaginated
	// for on-demand observability when traces are disabled globally.
	// Protected by lastTraceMu — written from a goroutine in
	// traceFinalize, read from the /lasttrace handler.
	lastTraceMu       sync.Mutex
	lastTraceSnapshot string

	// Turn batching — background agents (memory, mood, introspection)
	// accumulate skipped turns here until a substance gate fires or the
	// counter hits the threshold. Protected by pendingMu; turnCounter
	// is atomic for lock-free reads in the hot path.
	turnCounter  atomic.Int32
	pendingTurns []PendingTurn
	pendingMu    sync.Mutex

	// gatewayCmds holds gateway-level command handlers registered by
	// the Telegram adapter. handleMessage checks these before falling
	// through to the agent pipeline. This replaces the individual
	// tb.Handle("/cmd", ...) registrations for migrated commands.
	gatewayCmds []GatewayCommand

	// ownerChat is the Telegram chat ID for the bot owner. Used by
	// handleAgentEvent to send replies from event-triggered agent runs.
	ownerChat int64

	// workerCallback fires the worker agent in a background goroutine.
	// Set by cmd/run.go after bot creation — the bot doesn't import
	// workeragent directly (avoids import cycle).
	workerCallback func(taskType, note string, triggerMsgID int64)

	// workerCallbackSync runs the worker synchronously and returns its
	// summary. Used by send_task(wait=true) so the driver can block
	// until the worker finishes.
	workerCallbackSync func(taskType, note string, triggerMsgID int64) string

	// gmailBridge provides read-only Gmail access. Nil when Gmail is
	// not configured (no credentials in config). Set by cmd/run.go.
	gmailBridge gmail.Bridge

	// healthMonitor tracks Telegram activity and notifies systemd watchdog.
	// Started in Start() and stopped in Stop(). Logs warnings if the bot
	// receives no updates for extended periods (potential hung connection).
	healthMonitor       *HealthMonitor
	healthMonitorCtx    context.Context
	healthMonitorCancel context.CancelFunc
}

// PendingTurn stores the data needed to run background agents on a
// deferred turn. When the substance gate says "skip", the turn's key
// fields are stashed here so the memory agent can process them later
// when either a substantive turn arrives or the batch threshold fires.
//
// Think of this like a Python deque of pending work items — each one
// holds just enough context to replay the background agent pipeline.
type PendingTurn struct {
	UserMessage    string
	ReplyText      string
	ThinkTraces    []string
	TriggerMsgID   int64
	ConversationID string
}

// SetOwnerChat sets the chat ID for event-triggered agent replies.
// The owner chat is where scheduled tasks, skill failure notifications,
// and other non-user-initiated messages get sent.
func (b *Bot) SetOwnerChat(chatID int64) {
	b.ownerChat = chatID
}

// SetTTSClient sets the TTS client for voice synthesis. Called from
// gateway/pipeline.go to wire TTS into dev-mode bots (sims, Gradio).
func (b *Bot) SetTTSClient(c *voice.TTSClient) {
	b.ttsClient = c
}

// SetWorkerCallback sets the function that fires the worker agent in a
// background goroutine. Called from cmd/run.go after bot creation to
// avoid an import cycle (bot doesn't import workeragent).
func (b *Bot) SetWorkerCallback(cb func(taskType, note string, triggerMsgID int64)) {
	b.workerCallback = cb
}

// SetWorkerCallbackSync sets the synchronous worker dispatch function.
// Used by send_task(wait=true) — the driver blocks until the worker
// finishes and gets the summary inline as a tool result.
func (b *Bot) SetWorkerCallbackSync(cb func(taskType, note string, triggerMsgID int64) string) {
	b.workerCallbackSync = cb
}

// SetGmailBridge injects a Gmail bridge for email access.
func (b *Bot) SetGmailBridge(bridge gmail.Bridge) {
	b.gmailBridge = bridge
}

// AgentEventChannel returns a write-only channel for emitting agent events.
// The scheduler and skill runner use this to trigger agent runs without
// a user message. cmd/run.go passes this to their callbacks.
func (b *Bot) AgentEventChannel() chan<- agent.AgentEvent {
	return b.agentEvents
}

// New creates and configures a new Telegram bot.
func New(cfg *config.Config, configPath string, llmClient *llm.Client, driverLLM *llm.Client, memoryAgentLLM *llm.Client, moodAgentLLM *llm.Client, visionLLM *llm.Client, classifierLLM *llm.Client, dreamAgentLLM *llm.Client, introspectionLLM *llm.Client, embedClient *embed.Client, tavilyClient *search.TavilyClient, voiceClient *voice.Client, ttsClient *voice.TTSClient, store memory.Store, eventBus *tui.Bus) (*Bot, error) {
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
	var (
		tb  *tele.Bot
		err error
	)
	err = retry.Do(context.Background(), retry.Config{
		MaxAttempts: 3,
		Backoff:     retry.Exponential,
		InitialWait: 1 * time.Second,
	}, func() error {
		var e error
		tb, e = tele.NewBot(settings)
		return e
	})
	if err != nil {
		return nil, fmt.Errorf("creating telegram bot: %w", err)
	}

	// Load the base system prompt from prompt.md.
	promptBytes, err := os.ReadFile(cfg.Persona.PromptFile)
	if err != nil {
		return nil, fmt.Errorf("reading system prompt from %s: %w", cfg.Persona.PromptFile, err)
	}

	bot := &Bot{
		tb:               tb,
		llm:              llmClient,
		driverLLM:        driverLLM,
		memoryAgentLLM:   memoryAgentLLM,
		moodAgentLLM:     moodAgentLLM,
		visionLLM:        visionLLM,
		classifierLLM:    classifierLLM,
		dreamAgentLLM:    dreamAgentLLM,
		introspectionLLM: introspectionLLM,
		embedClient:      embedClient,
		tavilyClient:     tavilyClient,
		voiceClient:      voiceClient,
		ttsClient:        ttsClient,
		store:            store,
		cfg:              cfg,
		configPath:       configPath,
		systemPrompt:     string(promptBytes),
		startTime:        time.Now(),
		eventBus:         eventBus,
		agentEvents:      make(chan agent.AgentEvent, 16),
		shutdownCh:       make(chan struct{}),
		healthMonitor:    NewHealthMonitor(),
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

	// Register Telegram-specific command handlers. These commands use
	// Telegram UI features (inline buttons, multi-step progress, process
	// management) that can't be expressed as simple (string, error) returns.
	//
	// All other commands (/help, /stats, /facts, /forget, /traces,
	// /status, /reflect, /reflections, /persona, /dream, /dreamlog,
	// /lasttrace, /clear, /compact) are handled by the gateway command
	// system — they're intercepted in handleMessage before hitting the
	// agent pipeline. See tryGatewayCommand().
	tb.Handle("/mood", cmd("/mood", bot.handleMoodCommand))
	tb.Handle("/update", cmd("/update", bot.handleUpdate))
	tb.Handle("/restart", cmd("/restart", bot.handleRestart))

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

// NewDev creates a Bot configured for local dev mode — no Telegram
// connection, no bot token required. The Bot's agent pipeline, store,
// LLM clients, and event bus all work normally. Use ProcessMessage()
// to run turns from the HTTP dev server.
//
// This is the same Bot struct with the same agent pipeline — just no
// Telegram transport. Like creating a Python class with all methods
// working but the network socket set to None.
func NewDev(cfg *config.Config, configPath string, llmClient *llm.Client, driverLLM *llm.Client, memoryAgentLLM *llm.Client, moodAgentLLM *llm.Client, visionLLM *llm.Client, classifierLLM *llm.Client, dreamAgentLLM *llm.Client, introspectionLLM *llm.Client, embedClient *embed.Client, tavilyClient *search.TavilyClient, store memory.Store, eventBus *tui.Bus) (*Bot, error) {
	promptBytes, err := os.ReadFile(cfg.Persona.PromptFile)
	if err != nil {
		return nil, fmt.Errorf("reading system prompt from %s: %w", cfg.Persona.PromptFile, err)
	}

	b := &Bot{
		// tb is nil — no Telegram connection in dev mode
		llm:              llmClient,
		driverLLM:        driverLLM,
		memoryAgentLLM:   memoryAgentLLM,
		moodAgentLLM:     moodAgentLLM,
		visionLLM:        visionLLM,
		classifierLLM:    classifierLLM,
		dreamAgentLLM:    dreamAgentLLM,
		introspectionLLM: introspectionLLM,
		embedClient:      embedClient,
		tavilyClient:     tavilyClient,
		store:            store,
		cfg:              cfg,
		configPath:       configPath,
		systemPrompt:     string(promptBytes),
		startTime:        time.Now(),
		eventBus:         eventBus,
		agentEvents:      make(chan agent.AgentEvent, 16),
		shutdownCh:       make(chan struct{}),
	}

	// Wire the mood runner in dev mode too — same as Telegram New().
	// The Propose callback won't fire (no Telegram to send proposals
	// to), but mood logging and TUI events work normally.
	if moodAgentLLM != nil {
		if err := b.initMood(); err != nil {
			return nil, fmt.Errorf("initializing mood pipeline: %w", err)
		}
	}

	return b, nil
}

// SetCalendarBridge injects a calendar bridge for sim/test use.
// In production the calendar tools create their own CLIBridge; in sims
// we inject a FakeBridge so calendar operations work without Swift/EventKit.
func (b *Bot) SetCalendarBridge(bridge calendar.Bridge) {
	b.calendarBridge = bridge
}

// SetSimRun marks this bot as running in sim mode. Used by the gateway
// sim adapter so reply_direct's sim-only guard can verify context.
func (b *Bot) SetSimRun(v bool) { b.isSimRun = v }

// ProcessMessage runs a user message through the full agent pipeline
// using the given Frontend for I/O. This is the transport-agnostic
// entry point — the HTTP dev server and any future frontends call this
// instead of going through Telegram's handleMessage.
// MessageInput holds the fields for ProcessMessage. Text and
// ConversationID are required; image fields are optional.
type MessageInput struct {
	Text           string
	ConversationID string
	ImageBase64    string
	ImageMIME      string
}

func (b *Bot) ProcessMessage(fe Frontend, userText, conversationID string) (string, error) {
	return b.ProcessMessageInput(fe, MessageInput{
		Text:           userText,
		ConversationID: conversationID,
	})
}

func (b *Bot) ProcessMessageInput(fe Frontend, input MessageInput) (string, error) {
	log.Info("─── incoming message ───")
	log.Infof("  user: %s", truncate(input.Text, 100))

	msgID, err := b.store.SaveMessage("user", input.Text, "", input.ConversationID)
	if err != nil {
		log.Error("saving message", "err", err)
	}

	scrubResult := b.scrubText(input.Text)
	if msgID > 0 {
		b.store.UpdateMessageScrubbed(msgID, scrubResult.Text)
		b.savePIIVaultEntries(msgID, scrubResult.Vault)
	}

	err = b.runAgent(fe, AgentInput{
		UserMessage:    input.Text,
		ScrubbedText:   scrubResult.Text,
		ScrubVault:     scrubResult.Vault,
		ConversationID: input.ConversationID,
		TriggerMsgID:   msgID,
		ImageBase64:    input.ImageBase64,
		ImageMIME:      input.ImageMIME,
	})
	if err != nil {
		return "", err
	}

	return fe.ReplyText(), nil
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
	} else {
		// Verify the webhook is registered with Telegram. Self-heals
		// from manual deletions or failed deploys on previous runs.
		if err := b.verifyWebhookRegistration(); err != nil {
			log.Warn("webhook verification failed — updates may not arrive", "err", err)
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

	// Start the health monitor. This goroutine tracks Telegram activity
	// and notifies systemd watchdog to prevent silent hangs of the
	// long-polling connection.
	b.healthMonitorCtx, b.healthMonitorCancel = context.WithCancel(context.Background())
	go b.healthMonitor.Start(b.healthMonitorCtx)

	// Check for a pending restart flag. If the previous process wrote a
	// flag before dying, edit the "Restarting..." message to confirm
	// we're back. Runs before Start() so the edit arrives before any
	// user messages are processed.
	b.CheckRestartPending()

	// Notify systemd that we're ready to accept messages. This must be
	// called when using Type=notify, otherwise systemd waits indefinitely
	// and times out after 90 seconds (DefaultTimeoutStartSec).
	b.healthMonitor.NotifyReady()

	log.Info("Bot is running. Listening for messages...")
	b.tb.Start()
}

// Stop gracefully shuts down the bot. Safe to call multiple times —
// only the first call will execute the shutdown sequence.
func (b *Bot) Stop() {
	b.stopOnce.Do(func() {
		if b.moodSweeperStop != nil {
			b.moodSweeperStop() // cancels the sweeper goroutine
		}
		if b.healthMonitorCancel != nil {
			b.healthMonitorCancel() // stops the health monitor goroutine
		}
		b.agentEventsStopped.Store(true) // prevent sends before channel close
		close(b.shutdownCh)              // unblocks wizard expiry + other goroutines
		if b.tb != nil {
			b.tb.Stop() // stop accepting new Telegram updates
		}
		close(b.agentEvents) // signals consumeAgentEvents goroutine to exit
	})
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
		log.Info("handling scheduled event", "task", evt.TaskName, "schedule_id", evt.ScheduleID)

		// Inject schedule metadata so the agent knows which schedule triggered this.
		// This enables natural deletion: "delete this reminder" → bot knows the schedule ID.
		if evt.ScheduleID > 0 {
			prompt = fmt.Sprintf("%s\n\n[context: This message was triggered by schedule #%d (%q). "+
				"If the user asks to delete/remove/cancel this reminder or schedule, use delete_schedule with task_id=%d]",
				evt.Prompt, evt.ScheduleID, evt.TaskName, evt.ScheduleID)
		}

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

	case agent.EventWorkerComplete:
		log.Info("worker complete event", "task", evt.TaskName, "url", evt.ReportURL)
		reportRef := ""
		if evt.ReportURL != "" {
			reportRef = fmt.Sprintf("\n\nPublished at: %s", evt.ReportURL)
		}
		prompt = fmt.Sprintf(
			"[system] Your worker agent just finished a %s report.\n\n"+
				"Summary: %s%s\n\n"+
				"Share this with the user naturally — comment on what's interesting, "+
				"add your perspective. The report link will be attached automatically. "+
				"Keep it conversational, not like a system notification.",
			evt.TaskName, evt.Summary, reportRef,
		)
		conversationID = "worker-report"

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
	params.ScrubVault = &scrub.Vault{} // empty vault — event prompts have no PII tokens
	params.ConversationID = conversationID
	params.StatusCallback = sendFn
	params.SendCallback = sendFn

	// Wire TTS so event-triggered replies get narrated like regular turns.
	if b.ttsClient != nil {
		params.TTSCallback = func(text string) {
			b.sendVoiceReply2(ownerChat, text)
		}
	}

	b.agentBusy.Store(true)
	result, err := agent.Run(params)
	b.agentBusy.Store(false)

	if err != nil {
		log.Error("agent error from event", "type", evt.Type, "err", err)
		return
	}

	// Auto-append the report link or filename after the agent's reply.
	if evt.Type == agent.EventWorkerComplete {
		if evt.ReportURL != "" {
			linkMsg := fmt.Sprintf("📄 <a href=\"%s\">Read the full report</a>", evt.ReportURL)
			if err := b.SendToChat(b.ownerChat, linkMsg); err != nil {
				log.Error("failed to send report link", "err", err)
			}
		} else if result.ReplyText != "" {
			// No Telegraph URL — mention the local file so the user knows it exists.
			if reports, _ := filepath.Glob(filepath.Join(b.reportsDir(), "*.md")); len(reports) > 0 {
				latest := reports[len(reports)-1]
				fileMsg := fmt.Sprintf("📄 Report saved: <code>%s</code>", filepath.Base(latest))
				_ = b.SendToChat(b.ownerChat, fileMsg)
			}
		}
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
	// Record that we received a Telegram update for health monitoring.
	// This must be called early so we track activity even if the message
	// is intercepted by a wizard or command handler.
	b.healthMonitor.RecordUpdate()

	// Intercept text replies when a /mood wizard is waiting on its
	// note step. The wizard handler writes the entry + acknowledges
	// via edit; we return early so the message doesn't flow through
	// the agent pipeline.
	if b.HandleMoodWizardNote(c) {
		return nil
	}

	// Check for gateway commands (/help, /stats, /facts, etc.).
	// These are handled by the unified command system — same Exec*
	// methods that Gradio and other adapters use.
	if result, handled := b.tryGatewayCommand(c.Message().Text, c.Message().Chat.ID); handled {
		if strings.HasPrefix(result, "<") {
			return c.Send(result, &tele.SendOptions{ParseMode: tele.ModeHTML})
		}
		return c.Send(result)
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
	return b.runAgent(NewTelegramFrontend(c, b), AgentInput{
		UserMessage:    userText,
		ScrubbedText:   scrubResult.Text,
		ScrubVault:     scrubResult.Vault,
		ConversationID: conversationID,
		TriggerMsgID:   msgID,
	})
}
