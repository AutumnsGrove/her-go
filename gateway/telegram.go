package gateway

import (
	"context"

	"her/bot"
	"her/config"
	"her/memory"
	"her/tui"
)

// telegramAdapter implements the Adapter interface for Telegram.
// Unlike Gradio (a pull adapter where the gateway routes messages),
// Telegram is a push adapter — telebot registers handlers that call
// into bot.Bot directly. The gateway manages lifecycle (start/stop)
// and config; the bot handles everything else.
//
// This is the Phase 2a approach: zero changes to bot/. The adapter
// wraps bot.New() and delegates all Telegram handling to it. Phase 2b+
// will gradually extract handler logic into gateway-level commands.
type telegramAdapter struct {
	cfg  config.AdapterConfig
	gcfg *config.Config
	deps Deps
	bus  *tui.Bus

	bot   *bot.Bot
	store memory.Store
}

func newTelegramAdapter(acfg config.AdapterConfig, gcfg *config.Config, deps Deps, store memory.Store, bus *tui.Bus) (Adapter, error) {
	// Use token from adapter config, fall back to top-level telegram config.
	token := acfg.Token
	if token == "" {
		token = gcfg.Telegram.Token
	}

	// Build a config copy with the adapter's token (in case it differs).
	cfgCopy := *gcfg
	cfgCopy.Telegram.Token = token
	if acfg.Mode != "" {
		cfgCopy.Telegram.Mode = acfg.Mode
	}

	tgBot, err := bot.New(
		&cfgCopy, deps.ConfigPath,
		deps.ChatLLM, deps.DriverLLM,
		deps.MemoryAgentLLM, deps.MoodAgentLLM,
		deps.VisionLLM, deps.ClassifierLLM,
		deps.DreamAgentLLM, deps.IntrospectionLLM,
		deps.EmbedClient, deps.TavilyClient,
		deps.VoiceClient, deps.TTSClient,
		store, bus,
	)
	if err != nil {
		return nil, err
	}
	tgBot.SetOwnerChat(gcfg.Telegram.OwnerChat)

	return &telegramAdapter{
		cfg:   acfg,
		gcfg:  gcfg,
		deps:  deps,
		bus:   bus,
		bot:   tgBot,
		store: store,
	}, nil
}

func (a *telegramAdapter) Name() string { return a.cfg.Name }

func (a *telegramAdapter) Capabilities() CapSet {
	return CapSet{
		Edit:     true,
		Stream:   a.gcfg.Chat.Streaming,
		Paginate: true,
		Typing:   true,
		Audio:    a.deps.VoiceClient != nil,
		Confirm:  true,
	}
}

// Start blocks on telebot's polling/webhook loop. The gateway calls
// this in a goroutine. All message handling happens inside bot.Bot's
// registered handlers — no messages flow through Receive/Send.
func (a *telegramAdapter) Start(ctx context.Context) error {
	done := make(chan struct{})
	go func() {
		a.bot.Start()
		close(done)
	}()

	select {
	case <-ctx.Done():
		a.bot.Stop()
		<-done
		return nil
	case <-done:
		return nil
	}
}

func (a *telegramAdapter) Stop() error {
	a.bot.Stop()
	return nil
}

// Receive returns a closed channel. Telegram is a push adapter —
// messages are handled directly by telebot handlers inside bot.Bot,
// not routed through the gateway's message loop.
func (a *telegramAdapter) Receive() <-chan InboundMsg {
	ch := make(chan InboundMsg)
	close(ch)
	return ch
}

// Send is unused for Telegram — replies go through TelegramFrontend
// inside bot.Bot's handler flow.
func (a *telegramAdapter) Send(msg OutboundMsg) error  { return nil }
func (a *telegramAdapter) SendStatus(text string) error { return nil }
func (a *telegramAdapter) StartTyping() func()          { return func() {} }
func (a *telegramAdapter) OnTraceEvent(evt TraceEvent)  {}
func (a *telegramAdapter) RegisterCommands(cmds []CommandDef) {
	a.bot.RegisterGatewayCommands(toGatewayCmds(cmds))
}

// Engine returns the underlying bot for scheduler/dreamer wiring.
func (a *telegramAdapter) Engine() *bot.Bot { return a.bot }

// toGatewayCmds converts gateway CommandDefs to bot.GatewayCommands.
func toGatewayCmds(cmds []CommandDef) []bot.GatewayCommand {
	out := make([]bot.GatewayCommand, len(cmds))
	for i, c := range cmds {
		out[i] = bot.GatewayCommand{Name: c.Name, Handler: c.Handler}
	}
	return out
}
