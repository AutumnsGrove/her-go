package gateway

import (
	"context"
	"fmt"
	"sync"

	"her/bot"
	"her/config"
	"her/memory"
	"her/tui"
)

// Pipeline wraps the existing bot package as a transport-neutral
// message processor. In Phase 1 this is a thin shim — it translates
// between gateway types and bot.ProcessMessage(). The full bot/
// refactor (Phase 2) will replace this with direct agent pipeline calls.
type Pipeline struct {
	bot   *bot.Bot
	store memory.Store
	cfg   *config.Config
	bus   *tui.Bus
}

// NewPipeline creates a Pipeline by constructing a dev-mode Bot
// (no Telegram connection). The Bot's agent pipeline, LLM clients,
// and store all work normally — only the transport layer is absent.
func NewPipeline(cfg *config.Config, deps Deps, store memory.Store, bus *tui.Bus) (*Pipeline, error) {
	b, err := bot.NewDev(
		cfg, deps.ConfigPath,
		deps.ChatLLM, deps.DriverLLM,
		deps.MemoryAgentLLM, deps.MoodAgentLLM,
		deps.VisionLLM, deps.ClassifierLLM,
		deps.DreamAgentLLM, deps.IntrospectionLLM,
		deps.EmbedClient, deps.TavilyClient,
		store, bus,
	)
	if err != nil {
		return nil, fmt.Errorf("creating dev bot for pipeline: %w", err)
	}

	if deps.CalendarBridge != nil {
		b.SetCalendarBridge(deps.CalendarBridge)
	}
	if deps.WorkerCallback != nil {
		b.SetWorkerCallback(deps.WorkerCallback)
	}
	if deps.WorkerCallbackSync != nil {
		b.SetWorkerCallbackSync(deps.WorkerCallbackSync)
	}
	if deps.GmailBridge != nil {
		b.SetGmailBridge(deps.GmailBridge)
	}
	if deps.TTSClient != nil {
		b.SetTTSClient(deps.TTSClient)
	}

	return &Pipeline{
		bot:   b,
		store: store,
		cfg:   cfg,
		bus:   bus,
	}, nil
}

// Engine returns the underlying bot for adapters that need direct
// access to handler methods (e.g., Telegram adapter calls
// ProcessMessage with its own Frontend implementation).
func (p *Pipeline) Engine() *bot.Bot { return p.bot }

// Store returns the pipeline's memory store for command logging
// and other adapter-level operations.
func (p *Pipeline) Store() memory.Store { return p.store }

// Process runs an inbound message through the agent pipeline and
// returns the result. The adapter is used to provide Frontend
// capabilities (typing, status updates) during processing.
func (p *Pipeline) Process(ctx context.Context, msg InboundMsg, adapter Adapter) (OutboundMsg, error) {
	fe := &gatewayFrontend{adapter: adapter}

	replyText, err := p.bot.ProcessMessageInput(fe, bot.MessageInput{
		Text:           msg.Text,
		ConversationID: msg.ConversationID,
		ImageBase64:    msg.ImageBase64,
		ImageMIME:      msg.ImageMIME,
	})
	if err != nil {
		return OutboundMsg{}, err
	}

	return OutboundMsg{Text: replyText}, nil
}

// gatewayFrontend implements bot.Frontend by delegating to a gateway
// Adapter. This bridges the existing bot code to the new adapter
// interface — the bot calls Frontend methods, and we translate them
// into Adapter calls.
//
// This is a temporary shim for Phase 1. In Phase 2, the bot package
// will work directly with gateway types and this bridge goes away.
type gatewayFrontend struct {
	adapter Adapter
	mu      sync.Mutex
	reply   string
}

func (f *gatewayFrontend) SendPlaceholder(text string, html bool) error {
	return f.adapter.SendStatus(text)
}

func (f *gatewayFrontend) EditStatus(text string) error {
	// For non-Telegram frontends, EditStatus is how the final reply
	// gets delivered (the reply tool edits the placeholder with the
	// response text). We accumulate it so ReplyText() returns the
	// reply to the caller, while also forwarding to the adapter.
	f.mu.Lock()
	f.reply = text
	f.mu.Unlock()
	return f.adapter.SendStatus(text)
}

func (f *gatewayFrontend) SendReply(text string) error {
	f.mu.Lock()
	if f.reply != "" {
		f.reply += "\n\n"
	}
	f.reply += text
	f.mu.Unlock()
	return nil
}

func (f *gatewayFrontend) SendPaginated(text string) error {
	return f.SendReply(text)
}

func (f *gatewayFrontend) SendConfirm(text string) (int64, error) {
	return 0, nil
}

func (f *gatewayFrontend) StageReset() error {
	return nil
}

func (f *gatewayFrontend) DeletePlaceholder() error {
	return nil
}

func (f *gatewayFrontend) StartTyping() func() {
	return f.adapter.StartTyping()
}

func (f *gatewayFrontend) SupportsStreaming() bool {
	return f.adapter.Capabilities().Stream
}

func (f *gatewayFrontend) OnStreamToken(token string) {}

func (f *gatewayFrontend) StopStream() {}

func (f *gatewayFrontend) SendBusy() error {
	return f.adapter.Send(OutboundMsg{
		Text:    "I'm still thinking about your last message — give me a moment.",
		IsError: true,
	})
}

func (f *gatewayFrontend) SendError(text string) error {
	return f.adapter.Send(OutboundMsg{Text: text, IsError: true})
}

func (f *gatewayFrontend) ReplyText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reply
}
