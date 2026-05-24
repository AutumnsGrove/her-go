package gateway

import (
	"context"
	"fmt"
	"sync"

	"her/config"
	"her/embed"
	"her/llm"
	"her/logger"
	"her/memory"
	"her/search"
	"her/tui"
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
	ConfigPath       string
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
	}
}

// RegisterCommand adds a gateway-level command available to all adapters.
// Call this before Run().
func (g *Gateway) RegisterCommand(def CommandDef) {
	g.commands = append(g.commands, def)
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

		adapter, err := g.createAdapter(acfg)
		if err != nil {
			return fmt.Errorf("creating adapter %q: %w", acfg.Name, err)
		}
		if adapter == nil {
			log.Infof("adapter %q (type=%s) not yet implemented in gateway, skipping", acfg.Name, acfg.Type)
			continue
		}

		store, err := g.getOrCreateStore(acfg)
		if err != nil {
			return fmt.Errorf("opening store for adapter %q: %w", acfg.Name, err)
		}

		pipeline, err := NewPipeline(g.cfg, g.deps, store, g.bus)
		if err != nil {
			return fmt.Errorf("creating pipeline for adapter %q: %w", acfg.Name, err)
		}

		adapter.RegisterCommands(g.commands)

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
		<-ctx.Done()
		return nil
	}

	g.mu.Lock()
	g.adapters = started
	g.mu.Unlock()

	log.Infof("%d adapter(s) running", len(started))

	<-ctx.Done()
	return g.Stop()
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
// Returns (nil, nil) for adapter types that aren't implemented yet
// (e.g., "telegram" is handled by the legacy bot path in Phase 1).
func (g *Gateway) createAdapter(acfg config.AdapterConfig) (Adapter, error) {
	switch acfg.Type {
	case "gradio":
		return newGradioAdapter(acfg)
	case "telegram":
		// Phase 1: Telegram is handled by the legacy bot/ path.
		// Phase 2 will add a Telegram adapter here.
		return nil, nil
	default:
		return nil, fmt.Errorf("unknown adapter type: %q", acfg.Type)
	}
}
