package gateway

import (
	"context"
	"fmt"
	"sync"

	"her/bot"
	"her/calendar"
	"her/config"
	"her/embed"
	"her/llm"
	"her/logger"
	"her/memory"
	"her/search"
	"her/tui"
	"her/voice"
)

var log = logger.WithPrefix("gateway")

// Deps holds shared dependencies that are common across all adapters.
// LLM clients, embedding, and search are stateless — they don't need
// to be duplicated per adapter. Only the Store varies (keyed by DB path).
type Deps struct {
	ChatLLM          *llm.Client
	DriverLLM        *llm.Client
	MemoryAgentLLM   *llm.Client
	MoodAgentLLM     *llm.Client
	VisionLLM        *llm.Client
	ClassifierLLM    *llm.Client
	DreamAgentLLM    *llm.Client
	IntrospectionLLM *llm.Client
	EmbedClient      *embed.Client
	TavilyClient     *search.TavilyClient
	VoiceClient      *voice.Client    // STT — nil if voice disabled
	TTSClient        *voice.TTSClient // TTS — nil if TTS disabled
	CalendarBridge   calendar.Bridge              // nil in prod, FakeBridge in sims
	ConfigPath       string
	WorkerCallback   func(taskType, note string) // nil-safe — fires worker agent in background

	// WorkerResultCh receives worker completion data in sim mode.
	// The sim adapter reads it after each turn to inject follow-up turns.
	// Nil in production (events go through the bot's event channel).
	WorkerResultCh chan WorkerResult
}

// WorkerResult carries worker completion data for sim follow-up turns.
type WorkerResult struct {
	TaskName  string
	Summary   string
	ReportURL string
}

// Gateway is the top-level orchestrator. It manages adapter lifecycle,
// routes messages between adapters and the agent pipeline, and owns
// the command registry.
type Gateway struct {
	cfg      *config.Config
	deps     Deps
	bus      *tui.Bus
	commands []CommandDef

	// AdapterFilter, when non-empty, restricts which adapter types
	// are started. Set by the --adapter CLI flag.
	AdapterFilter string

	// stores is keyed by absolute DB path. Two adapters pointing to the
	// same her.db get the same Store pointer — shared memory falls out
	// of pointer equality, no special logic needed.
	stores map[string]memory.Store

	mu       sync.Mutex
	adapters []adapterEntry

	// Ready is closed when all adapters have been created (before they
	// start blocking). Callers can <-gw.Ready to wait for adapters to
	// be available for TelegramBot() etc.
	Ready chan struct{}

	// SimMessages, when set, provides the message sequence for a sim
	// adapter. Set by the sim command before calling Run().
	SimMessages []SimMessage
	SimTriggers SimTriggers
	SimOptions  SimOptions
}

// adapterEntry pairs an adapter with its pipeline for message routing.
type adapterEntry struct {
	adapter  Adapter
	pipeline *Pipeline
	cfg      config.AdapterConfig
}

// New creates a Gateway from config and shared dependencies.
func New(cfg *config.Config, deps Deps, bus *tui.Bus) *Gateway {
	return &Gateway{
		cfg:    cfg,
		deps:   deps,
		bus:    bus,
		stores: make(map[string]memory.Store),
		Ready:  make(chan struct{}),
	}
}

// RegisterCommand adds a gateway-level command available to all adapters.
// Call this before Run().
func (g *Gateway) RegisterCommand(def CommandDef) {
	g.commands = append(g.commands, def)
}

// RegisterStore pre-registers a store for a given DB path. The gateway
// will reuse this store instead of opening a new one. Used by the sim
// command to inject a pre-seeded database.
func (g *Gateway) RegisterStore(dbPath string, store memory.Store) {
	g.stores[dbPath] = store
}

// Run starts all enabled adapters and blocks until ctx is cancelled.
// Each adapter gets its own goroutine for receiving messages and routing
// them through the pipeline.
func (g *Gateway) Run(ctx context.Context) error {
	enabledAdapters := g.cfg.Gateway.Adapters
	if len(enabledAdapters) == 0 {
		return fmt.Errorf("no adapters configured in gateway.adapters")
	}

	var started []adapterEntry

	for _, acfg := range enabledAdapters {
		if !acfg.IsEnabled() {
			log.Infof("gateway: adapter %q disabled, skipping", acfg.Name)
			continue
		}

		if g.AdapterFilter != "" && acfg.Type != g.AdapterFilter {
			log.Infof("gateway: adapter %q filtered out (--adapter=%s)", acfg.Name, g.AdapterFilter)
			continue
		}

		store, err := g.getOrCreateStore(acfg)
		if err != nil {
			return fmt.Errorf("opening store for adapter %q: %w", acfg.Name, err)
		}

		adapter, err := g.createAdapter(acfg, store)
		if err != nil {
			return fmt.Errorf("creating adapter %q: %w", acfg.Name, err)
		}
		if adapter == nil {
			log.Infof("gateway: adapter %q (type=%s) not yet implemented, skipping", acfg.Name, acfg.Type)
			continue
		}

		// Gradio and other pull adapters need a pipeline for message routing.
		// Telegram is a push adapter — it handles messages internally via
		// bot.Bot, so pipeline is nil for it.
		var pipeline *Pipeline
		if acfg.Type != "telegram" {
			pipeline, err = NewPipeline(g.cfg, g.deps, store, g.bus)
			if err != nil {
				return fmt.Errorf("creating pipeline for adapter %q: %w", acfg.Name, err)
			}
			if acfg.Type == "sim" {
				pipeline.Engine().SetSimRun(true)
			}
		}

		// Register commands: gateway-level first, then pipeline-derived.
		// Pipeline commands wrap bot.Bot's Exec* methods so every slash
		// command works on every adapter — not just Telegram.
		cmds := append([]CommandDef{}, g.commands...)
		if pipeline != nil {
			cmds = append(cmds, buildCommands(pipeline)...)

			// Wire adapter-specific handlers that need pipeline access.
			b := pipeline.Engine()
			if ga, ok := adapter.(*gradioAdapter); ok {
				ga.compactHandler = func(ctx context.Context, convID string) (string, error) {
					return b.ExecCompact(convID)
				}
				pipelineStore := pipeline.Store()
				ga.logCommand = func(command, conversationID, args string) {
					pipelineStore.LogCommand(command, 0, conversationID, args)
				}
			}
			if sa, ok := adapter.(*simAdapter); ok {
				sa.compactHandler = func(ctx context.Context, convID string) (string, error) {
					return b.ExecCompact(convID)
				}
			}
		}

		// For the Telegram adapter, build commands from its own bot.Bot.
		// Telegram is a push adapter (no pipeline), but its bot has the
		// same Exec* methods — we just need to wire them up so
		// handleMessage can route commands through the gateway system.
		if ta, ok := adapter.(*telegramAdapter); ok {
			cmds = append(cmds, buildCommandsFromBot(ta.Engine())...)
		}
		adapter.RegisterCommands(cmds)

		entry := adapterEntry{
			adapter:  adapter,
			pipeline: pipeline,
			cfg:      acfg,
		}
		started = append(started, entry)

		log.Infof("gateway: starting adapter %q (type=%s, db=%s)", acfg.Name, acfg.Type, acfg.DB)

		go g.runAdapter(ctx, entry)
		go func(a Adapter, c context.Context) {
			if err := a.Start(c); err != nil && c.Err() == nil {
				log.Errorf("gateway: adapter %q exited with error: %v", a.Name(), err)
			}
		}(adapter, ctx)
	}

	if len(started) == 0 {
		log.Infof("no gateway adapters started (all legacy or disabled)")
		close(g.Ready)
		<-ctx.Done()
		return nil
	}

	g.mu.Lock()
	g.adapters = started
	g.mu.Unlock()
	close(g.Ready) // signal that adapters are available

	log.Infof("%d adapter(s) running", len(started))

	<-ctx.Done()
	return g.Stop()
}

// TelegramBot returns the bot.Bot from the Telegram adapter, if one
// is running. Returns nil if no Telegram adapter is active. Used by
// cmd/run.go to wire the scheduler's Send callback.
func (g *Gateway) TelegramBot() *bot.Bot {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, e := range g.adapters {
		if ta, ok := e.adapter.(*telegramAdapter); ok {
			return ta.Engine()
		}
	}
	return nil
}

// SimAdapter returns the sim adapter if one is running.
// Used by the sim command to wait for completion and read results.
func (g *Gateway) SimAdapter() *simAdapter {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, e := range g.adapters {
		if sa, ok := e.adapter.(*simAdapter); ok {
			return sa
		}
	}
	return nil
}

// Stop gracefully shuts down all adapters.
func (g *Gateway) Stop() error {
	g.mu.Lock()
	entries := g.adapters
	g.mu.Unlock()

	var firstErr error
	for _, e := range entries {
		if err := e.adapter.Stop(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// runAdapter reads messages from an adapter and routes them through
// the pipeline. Runs in its own goroutine per adapter.
func (g *Gateway) runAdapter(ctx context.Context, entry adapterEntry) {
	msgCh := entry.adapter.Receive()
	for {
		select {
		case <-ctx.Done():
			return
		case msg, ok := <-msgCh:
			if !ok {
				return
			}
			g.handleMessage(ctx, entry, msg)
		}
	}
}

// handleMessage processes a single inbound message through the pipeline
// and sends the result back via the adapter.
func (g *Gateway) handleMessage(ctx context.Context, entry adapterEntry, msg InboundMsg) {
	log.Infof("gateway: [%s] message from conversation %s", entry.adapter.Name(), msg.ConversationID)

	result, err := entry.pipeline.Process(ctx, msg, entry.adapter)
	if err != nil {
		log.Errorf("gateway: pipeline error for adapter %q: %v", entry.adapter.Name(), err)
		_ = entry.adapter.Send(OutboundMsg{
			Text:    "Something went wrong. Try again in a moment.",
			IsError: true,
		})
		return
	}

	if err := entry.adapter.Send(result); err != nil {
		log.Errorf("gateway: send error for adapter %q: %v", entry.adapter.Name(), err)
	}
}

// getOrCreateStore returns an existing store for the given DB path,
// or creates a new one. Two adapters with the same DB path share
// one Store instance.
func (g *Gateway) getOrCreateStore(acfg config.AdapterConfig) (memory.Store, error) {
	dbPath := acfg.DB
	if dbPath == "" {
		dbPath = g.cfg.Memory.DBPath
	}

	if store, ok := g.stores[dbPath]; ok {
		log.Infof("gateway: reusing store for %s", dbPath)
		return store, nil
	}

	store, err := memory.NewStore(dbPath, g.cfg.Embed.Dimension)
	if err != nil {
		return nil, fmt.Errorf("opening database %s: %w", dbPath, err)
	}

	g.stores[dbPath] = store
	return store, nil
}

// createAdapter builds an Adapter from config. This is the adapter
// factory — add new adapter types here as they're implemented.
func (g *Gateway) createAdapter(acfg config.AdapterConfig, store memory.Store) (Adapter, error) {
	switch acfg.Type {
	case "telegram":
		return newTelegramAdapter(acfg, g.cfg, g.deps, store, g.bus)
	case "gradio":
		return newGradioAdapter(acfg, g.bus)
	case "sim":
		return newSimAdapter(acfg, g.SimMessages, g.SimTriggers, g.SimOptions, g.bus, g.deps.WorkerResultCh)
	default:
		return nil, fmt.Errorf("unknown adapter type: %q", acfg.Type)
	}
}
