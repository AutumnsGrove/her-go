package cmd

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"net/http"

	"her/agent"
	"her/bot"
	"her/config"
	"her/d1"
	"her/embed"
	"her/gateway"
	"her/llm"
	"her/logger"
	"her/memory"
	"her/persona"
	"her/procmgr"
	"her/retry"
	"her/scheduler"
	"her/search"
	"her/telegraph"
	"her/tui"
	"her/voice"

	// Blank-import scheduler extensions so their init() side-effects
	// (scheduler.Register) run. Each extension lives in its domain
	// package; we pull them in here so the runtime knows about them
	// without the scheduler package itself depending on every
	// domain.
	_ "her/mood"
	"her/workeragent"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"gopkg.in/natefinch/lumberjack.v2"
)

// log is the package-level logger for the cmd package.
var log = logger.WithPrefix("cmd")

// adapterFilter is set by the --adapter flag. When non-empty, only the
// named adapter type is started (e.g., "gradio"). All others are skipped.
var adapterFilter string

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Start the bot process (foreground)",
	Long:  "Loads config, initializes the database and API clients, and runs the bot.\nThis blocks until the process receives SIGINT or SIGTERM.\n\nUse --adapter to start only a specific adapter (e.g., --adapter=gradio).\nOr use 'her run gradio' as a shortcut for '--adapter=gradio'.",
	Args:  cobra.MaximumNArgs(1),
	RunE: func(cmd *cobra.Command, args []string) error {
		if len(args) == 1 && adapterFilter == "" {
			adapterFilter = args[0]
		}
		return runBot(cmd, args)
	},
}

func init() {
	runCmd.Flags().StringVar(&adapterFilter, "adapter", "", "start only this adapter type (e.g., gradio)")
	rootCmd.AddCommand(runCmd)
}

// configTransform is an optional hook that modifies the config after loading.
// Set by `her dev` to override mode, db_path, etc. before the bot starts.
var configTransform func(*config.Config)

// devCleanup is called during shutdown if set. `her dev` uses this to clear
// KV routing keys so the CF Worker routes traffic back to prod.
var devCleanup func()

// runBot contains all the initialization and startup logic.
// With the TUI enabled, it:
//  1. Does fatal pre-checks (config, tokens, DB) — these fail before the TUI
//  2. Starts the event bus and logger bridge
//  3. Launches Bubble Tea on the main goroutine (it needs terminal control)
//  4. Runs all remaining init + the Telegram bot in a background goroutine
//  5. Events flow: init/bot/agent → bus → TUI + file logger
func runBot(cmd *cobra.Command, args []string) error {
	// --- Pre-TUI fatal checks (these exit before the TUI starts) ---

	cfg, err := config.Load(cfgFile)
	if err != nil {
		log.Fatal("Failed to load config", "err", err)
	}

	// Apply dev mode overrides if `her dev` set a transform.
	if configTransform != nil {
		configTransform(cfg)
	}

	// Export config secrets as process-level env vars so skills can find
	// them via os.Getenv(). These die with the process — never touch the
	// parent shell, no cleanup needed.
	cfg.ExportEnv()

	// Enable debug mode if configured — logs full API request/response bodies.
	if cfg.Debug {
		llm.SetDebugMode(true)
		log.Info("debug mode enabled — full API context will be logged")
	}

	// Telegram token only required when we're actually starting Telegram.
	// With --adapter=gradio (or other non-telegram), skip the check.
	skipTelegram := adapterFilter != "" && adapterFilter != "telegram"
	if cfg.Telegram.Token == "" && !skipTelegram {
		log.Fatal("Telegram token is required — set TELEGRAM_BOT_TOKEN env var or fill in config.yaml")
	}
	if cfg.OpenRouter.APIKey == "" {
		log.Fatal("LLM API key is required — set OPENROUTER_API_KEY env var or fill in config.yaml")
	}

	// If the managed service is running (`her start`), stop it before
	// we start a foreground run. Two instances racing for the same
	// Telegram token causes dropped messages and PID file conflicts.
	stopManagedServiceIfRunning(cfg)

	// Kill any stale her process from a previous run before we start.
	// This prevents two instances racing for the same Telegram token.
	const herPIDFile = "her.pid"
	killStaleSelf(herPIDFile)
	if err := os.WriteFile(herPIDFile, []byte(strconv.Itoa(os.Getpid())), 0644); err != nil {
		log.Fatal("failed to write PID file — cannot ensure single instance", "err", err)
	}
	defer os.Remove(herPIDFile)

	if dbDir := filepath.Dir(cfg.Memory.DBPath); dbDir != "." && dbDir != "" {
		if err := os.MkdirAll(dbDir, 0755); err != nil {
			log.Fatal("cannot create database directory", "path", dbDir, "err", err)
		}
	}
	store, err := memory.NewStore(cfg.Memory.DBPath, cfg.Embed.Dimension)
	if err != nil {
		log.Fatal("Failed to initialize database", "err", err)
	}
	store.AutoLinkCount = cfg.Memory.AutoLinkCount
	store.AutoLinkThreshold = cfg.Memory.AutoLinkThreshold

	store.ApplyRecallConfig(cfg.Memory.Recall)

	// If D1 sync is configured, wrap SQLiteStore in SyncedStore for
	// bidirectional sync with Cloudflare D1. Writes push to D1 in the
	// background; Pull() on startup hydrates local her.db with any
	// rows the other machine created since last sync.
	//
	// When D1 is disabled (empty d1_database_id), botStore is the plain
	// SQLiteStore — zero overhead, no D1 dependency.
	var botStore memory.Store = store
	if cfg.Cloudflare.D1DatabaseID != "" {
		d1Client := d1.NewClient(cfg.Cloudflare.AccountID, cfg.Cloudflare.D1DatabaseID, cfg.Cloudflare.APIToken)
		if d1Client != nil {
			synced, err := memory.NewSyncedStore(store, d1Client)
			if err != nil {
				log.Fatal("Failed to initialize D1 sync", "err", err)
			}

			// Apply sync tuning from config (zero values keep defaults).
			if cfg.Cloudflare.Sync.BatchSize > 0 {
				synced.BatchSize = cfg.Cloudflare.Sync.BatchSize
			}
			if cfg.Cloudflare.Sync.CarrierPollSeconds > 0 {
				synced.CarrierPoll = time.Duration(cfg.Cloudflare.Sync.CarrierPollSeconds) * time.Second
			}
			if cfg.Cloudflare.Sync.PullPageSize > 0 {
				synced.PullPageSize = cfg.Cloudflare.Sync.PullPageSize
			}

			// Pull from D1 before the bot starts — ensures we have all
			// data the other machine wrote since we last ran.
			startupTimeout := time.Duration(cfg.Cloudflare.Sync.StartupPullTimeout) * time.Second
			if startupTimeout == 0 {
				startupTimeout = 60 * time.Second
			}
			pullCtx, pullCancel := context.WithTimeout(context.Background(), startupTimeout)
			err = retry.Do(pullCtx, retry.Config{
				MaxAttempts: 3,
				Backoff:     retry.Exponential,
				InitialWait: 2 * time.Second,
			}, func() error {
				return synced.Pull(pullCtx)
			})
			if err != nil {
				log.Error("d1 pull on startup failed — continuing with local data (sync may be stale)", "err", err)
			}
			pullCancel()

			botStore = synced
			log.Info("d1 sync enabled")
		}
	}
	defer botStore.Close()

	// --- Start the event bus and logger bridge ---
	// From this point on, all log.Info/Warn/Error calls flow through the bus.

	bus := tui.NewBus()

	// Set up rotating log file via lumberjack. This replaces the simple
	// os.OpenFile approach — lumberjack handles rotation automatically when
	// the file exceeds MaxSize. The Logger struct implements io.Writer.
	//
	// Settings:
	//   MaxSize=10:    rotate at 10MB
	//   MaxBackups=0:  keep ALL old files (Autumn wants history preserved)
	//   MaxAge=0:      no age-based deletion
	//   LocalTime:     use local time in backup filenames
	//   Compress:      false — keep uncompressed for grep
	logFile := &lumberjack.Logger{
		Filename:   "logs/her.log",
		MaxSize:    10,
		MaxBackups: 0,
		MaxAge:     0,
		LocalTime:  true,
		Compress:   false,
	}

	// Rotate on startup: archives the previous session's log and starts fresh.
	// This gives us session-based log files (one per bot run) with MB-based
	// rotation as a safety valve for runaway sessions.
	// Rotated files get timestamped names like: her-2026-04-14T15-30-00.log
	if err := logFile.Rotate(); err != nil {
		// Non-fatal — worst case we append to the previous session's log
		log.Warn("could not rotate log file", "err", err)
	}

	// Only pass bus, not logFile — the StartFileLogger subscriber
	// handles writing events to the file. Passing logFile to Init
	// would cause double-writes (logger bridge + file subscriber
	// both writing to the same file).
	logger.Init(bus, nil)
	tui.StartFileLogger(bus, logFile)

	// Sidecar output goes to the log file in TUI mode so it doesn't
	// corrupt the alt screen. In plain mode it stays on stderr.
	sidecarOut = logFile

	// Emit structured SESSION_START for observability and future log splitting.
	// The date= and time= fields make it trivial to grep and split logs by session.
	log.Info("SESSION_START",
		"date", time.Now().Format("2006-01-02"),
		"time", time.Now().Format("15:04:05"),
		"driver_model", cfg.Driver.Model,
		"chat_model", cfg.Chat.Model,
		"classifier_model", cfg.Classifier.Model,
	)

	// Emit startup events now that the bus is live
	bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "db", Status: "ready", Detail: cfg.Memory.DBPath})

	// D1 sync status — let the TUI show whether Cloudflare is connected.
	if _, ok := botStore.(*memory.SyncedStore); ok {
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "d1_sync", Status: "ready", Detail: "cloudflare D1"})
	} else {
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "d1_sync", Status: "skipped"})
	}

	// --- Decide: TUI or plain fallback ---
	// If stdout is not a terminal (piped, redirected, CI), skip the TUI
	// and let the file logger handle everything.
	useTUI := term.IsTerminal(int(os.Stdout.Fd()))

	if !useTUI {
		// Plain mode — run init and bot directly on this goroutine
		return runBotPlain(cfg, botStore, bus)
	}

	// --- Start Bubble Tea TUI ---
	// The TUI needs the main goroutine for terminal control (raw mode,
	// alt screen, mouse capture). Everything else runs in goroutines.

	quitCh := make(chan struct{})
	eventCh := bus.Subscribe(256)
	model := tui.NewModel(eventCh, quitCh, cfg)
	program := tea.NewProgram(model)

	// Run all remaining initialization + the bot in a background goroutine.
	// This goroutine's lifecycle: init → run bot → wait for quit → cleanup.
	go func() {
		runBotBackground(cfg, botStore, bus, program, quitCh)
	}()

	// Block the main goroutine on the TUI.
	if _, err := program.Run(); err != nil {
		return err
	}

	return nil
}

// sidecarOut is the io.Writer where sidecar processes send their output.
// In TUI mode this is the log file (so it doesn't corrupt the screen).
// In plain mode this is stderr (same as before the TUI work).
var sidecarOut io.Writer = os.Stderr

// runBotBackground handles all init and bot lifecycle in a goroutine while
// the TUI runs on the main goroutine.
func runBotBackground(cfg *config.Config, store memory.Store, bus *tui.Bus, program *tea.Program, quitCh chan struct{}) {
	skipTelegram := adapterFilter != "" && adapterFilter != "telegram"

	// --- Create LLM clients ---

	llmClient := llm.NewClient(cfg.OpenRouter.BaseURL, cfg.OpenRouter.APIKey, cfg.Chat.Model, cfg.Chat.Temperature, cfg.Chat.MaxTokens)
	if cfg.Chat.Timeout > 0 {
		llmClient.WithTimeout(time.Duration(cfg.Chat.Timeout) * time.Second)
	}
	if cfg.Chat.Provider != nil {
		llmClient.WithProvider(&llm.ProviderRouting{Order: cfg.Chat.Provider.Order, Only: cfg.Chat.Provider.Only, Sort: cfg.Chat.Provider.Sort})
	}
	if cfg.Chat.Fallback != nil {
		llmClient.WithFallback(cfg.Chat.Fallback.Model, cfg.Chat.Fallback.Temperature, cfg.Chat.Fallback.MaxTokens)
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "llm", Status: "ready", Detail: cfg.Chat.Model + " (fallback: " + cfg.Chat.Fallback.Model + ")"})
	} else {
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "llm", Status: "ready", Detail: cfg.Chat.Model})
	}
	if cfg.Chat.Reasoning != nil && cfg.Chat.Reasoning.Enabled != nil {
		llmClient.WithReasoning(&llm.ReasoningControl{Enabled: cfg.Chat.Reasoning.Enabled})
	}

	driverClient := llm.NewClient(cfg.OpenRouter.BaseURL, cfg.OpenRouter.APIKey, cfg.Driver.Model, cfg.Driver.Temperature, cfg.Driver.MaxTokens)
	if cfg.Driver.Timeout > 0 {
		driverClient.WithTimeout(time.Duration(cfg.Driver.Timeout) * time.Second)
	}
	if cfg.Driver.Fallback != nil {
		driverClient.WithFallback(cfg.Driver.Fallback.Model, cfg.Driver.Fallback.Temperature, cfg.Driver.Fallback.MaxTokens)
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "driver", Status: "ready", Detail: cfg.Driver.Model + " (fallback: " + cfg.Driver.Fallback.Model + ")"})
	} else {
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "driver", Status: "ready", Detail: cfg.Driver.Model})
	}
	if cfg.Driver.Reasoning != nil && cfg.Driver.Reasoning.Enabled != nil {
		driverClient.WithReasoning(&llm.ReasoningControl{Enabled: cfg.Driver.Reasoning.Enabled})
	}

	var visionClient *llm.Client
	if cfg.Vision.Model != "" {
		visionClient = llm.NewClient(cfg.OpenRouter.BaseURL, cfg.OpenRouter.APIKey, cfg.Vision.Model, cfg.Vision.Temperature, cfg.Vision.MaxTokens)
		if cfg.Vision.Fallback != nil {
			visionClient.WithFallback(cfg.Vision.Fallback.Model, cfg.Vision.Fallback.Temperature, cfg.Vision.Fallback.MaxTokens)
		}
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "vision", Status: "ready", Detail: cfg.Vision.Model})
	} else {
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "vision", Status: "skipped"})
	}

	// --- Classifier client (optional) ---
	// Small, fast model that validates memory writes before they hit the DB.
	// Catches fictional content (game events, etc.) that the driver model
	// mistakes for real user facts.
	var classifierClient *llm.Client
	if cfg.Classifier.Model != "" {
		classifierClient = llm.NewClient(cfg.OpenRouter.BaseURL, cfg.OpenRouter.APIKey, cfg.Classifier.Model, cfg.Classifier.Temperature, cfg.Classifier.MaxTokens)
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "classifier", Status: "ready", Detail: cfg.Classifier.Model})
	} else {
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "classifier", Status: "skipped"})
	}

	// --- Memory agent client (optional) ---
	// Post-turn background agent that reviews conversation turns and extracts
	// facts. Runs in a goroutine after the reply is sent — never blocks the user.
	var memoryAgentClient *llm.Client
	if cfg.MemoryAgent.Model != "" {
		memoryAgentClient = llm.NewClient(cfg.OpenRouter.BaseURL, cfg.OpenRouter.APIKey, cfg.MemoryAgent.Model, cfg.MemoryAgent.Temperature, cfg.MemoryAgent.MaxTokens)
		if cfg.MemoryAgent.Timeout > 0 {
			memoryAgentClient.WithTimeout(time.Duration(cfg.MemoryAgent.Timeout) * time.Second)
		}
		if cfg.MemoryAgent.Provider != nil {
			memoryAgentClient.WithProvider(&llm.ProviderRouting{
				Order: cfg.MemoryAgent.Provider.Order,
				Only:  cfg.MemoryAgent.Provider.Only,
				Sort:  cfg.MemoryAgent.Provider.Sort,
			})
		}
		if cfg.MemoryAgent.Fallback != nil {
			memoryAgentClient.WithFallback(cfg.MemoryAgent.Fallback.Model, cfg.MemoryAgent.Fallback.Temperature, cfg.MemoryAgent.Fallback.MaxTokens)
			bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "memory_agent", Status: "ready", Detail: cfg.MemoryAgent.Model + " (fallback: " + cfg.MemoryAgent.Fallback.Model + ")"})
		} else {
			bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "memory_agent", Status: "ready", Detail: cfg.MemoryAgent.Model})
		}
		if cfg.MemoryAgent.Reasoning != nil && cfg.MemoryAgent.Reasoning.Enabled != nil {
			memoryAgentClient.WithReasoning(&llm.ReasoningControl{Enabled: cfg.MemoryAgent.Reasoning.Enabled})
		}
	} else {
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "memory_agent", Status: "skipped"})
	}

	// --- Mood agent client (optional) ---
	// Post-turn background agent that infers the user's state of mind.
	// Same shape as the memory agent: runs parallel in a goroutine,
	// never blocks the reply. Empty model disables it.
	var moodAgentClient *llm.Client
	if cfg.MoodAgent.Model != "" {
		mTokens := cfg.MoodAgent.MaxTokens
		if mTokens == 0 {
			mTokens = 512
		}
		mTemp := cfg.MoodAgent.Temperature
		if mTemp == 0 {
			mTemp = 0.2
		}
		moodAgentClient = llm.NewClient(cfg.OpenRouter.BaseURL, cfg.OpenRouter.APIKey, cfg.MoodAgent.Model, mTemp, mTokens)
		if cfg.MoodAgent.Timeout > 0 {
			moodAgentClient.WithTimeout(time.Duration(cfg.MoodAgent.Timeout) * time.Second)
		}
		if cfg.MoodAgent.Provider != nil {
			moodAgentClient.WithProvider(&llm.ProviderRouting{
				Order: cfg.MoodAgent.Provider.Order,
				Only:  cfg.MoodAgent.Provider.Only,
				Sort:  cfg.MoodAgent.Provider.Sort,
			})
		}
		if cfg.MoodAgent.Fallback != nil {
			moodAgentClient.WithFallback(cfg.MoodAgent.Fallback.Model, cfg.MoodAgent.Fallback.Temperature, cfg.MoodAgent.Fallback.MaxTokens)
		}
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "mood_agent", Status: "ready", Detail: cfg.MoodAgent.Model})
	} else {
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "mood_agent", Status: "skipped"})
	}

	// --- Embedding client ---
	// Client is always created here if configured. Health check + optional
	// auto-start happens in the sidecars section below alongside stt/tts.

	var embedClient *embed.Client
	if cfg.Embed.BaseURL != "" && cfg.Embed.Model != "" {
		embedClient = embed.NewClient(cfg.Embed.BaseURL, cfg.Embed.Model, cfg.Embed.APIKey, cfg.Embed.Dimension)
	} else {
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "embed", Status: "skipped"})
	}

	// Backfill embeddings in background
	if embedClient != nil && cfg.Embed.Dimension > 0 {
		go func() {
			unembedded, err := store.MemoriesWithoutEmbeddings()
			if err != nil {
				log.Error("backfill: failed to query unembedded memories", "err", err)
				return
			}
			if len(unembedded) == 0 {
				return
			}
			log.Infof("Backfilling embeddings for %d memories...", len(unembedded))
			for _, m := range unembedded {
				// Embed by tags when available (topic-based retrieval),
				// fall back to memory text for un-tagged memories.
				embedText := m.Tags
				if embedText == "" {
					embedText = m.Content
				}
				vec, err := embedClient.Embed(embedText)
				if err != nil {
					log.Error("backfill: embedding failed", "memory_id", m.ID, "err", err)
					continue
				}
				// Pass nil for embeddingText — this backfill only targets the tag
				// embedding (vec_memories). Text embeddings are populated on-demand
				// by checkDuplicate and FilterRedundantMemories.
				if err := store.UpdateMemoryEmbedding(m.ID, vec, nil); err != nil {
					log.Error("backfill: update failed", "memory_id", m.ID, "err", err)
					continue
				}
			}
			log.Infof("Backfill complete: %d memories embedded", len(unembedded))
		}()
	}

	// --- Search ---

	var tavilyClient *search.TavilyClient
	if cfg.Search.TavilyAPIKey != "" {
		tavilyClient = search.NewTavilyClient(cfg.Search.TavilyAPIKey, cfg.Search.TavilyBaseURL)
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "search", Status: "ready"})
	} else {
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "search", Status: "skipped"})
	}

	// --- Sidecars (STT/TTS) with pipe capture ---

	var sttProcess *exec.Cmd
	// Pass the OpenRouter key as a fallback so whisper STT works without
	// duplicating the key in the voice section of config.yaml.
	voiceClient := voice.NewClient(&cfg.Voice, cfg.OpenRouter.APIKey)
	if voiceClient != nil {
		sttProcess = startSTTSidecar(cfg, bus, voiceClient)
	} else {
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "stt", Status: "skipped"})
	}

	var ttsProcess *exec.Cmd
	ttsClient := voice.NewTTSClient(&cfg.Voice.TTS)
	if ttsClient != nil {
		ttsProcess = startTTSSidecar(cfg, bus, ttsClient)
	} else {
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "tts", Status: "skipped"})
	}

	var embedProcess *exec.Cmd
	if embedClient != nil {
		if embedClient.IsAvailable() {
			bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "embed", Status: "ready", Detail: cfg.Embed.Model})
		} else if cfg.Embed.StartCommand != "" {
			embedProcess = startEmbedSidecar(cfg, bus, embedClient)
		} else {
			log.Warn("embed server not responding — semantic recall degraded to keyword fallback")
			bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "embed", Status: "failed", Detail: "not responding"})
		}
	}

	// --- Dream agent client (optional) ---
	// Dedicated model for the memory dreamer — autonomous memory consolidation
	// that runs as Step 0 of the nightly dream cycle. Falls back to the memory
	// agent client if dream_agent.model isn't configured.
	var dreamAgentClient *llm.Client
	if cfg.DreamAgent.Model != "" {
		dreamAgentClient = llm.NewClient(cfg.OpenRouter.BaseURL, cfg.OpenRouter.APIKey, cfg.DreamAgent.Model, cfg.DreamAgent.Temperature, cfg.DreamAgent.MaxTokens)
		timeout := cfg.DreamAgent.Timeout
		if timeout == 0 {
			timeout = 120
		}
		dreamAgentClient.WithTimeout(time.Duration(timeout) * time.Second)
		if cfg.DreamAgent.Provider != nil {
			dreamAgentClient.WithProvider(&llm.ProviderRouting{
				Order: cfg.DreamAgent.Provider.Order,
				Only:  cfg.DreamAgent.Provider.Only,
				Sort:  cfg.DreamAgent.Provider.Sort,
			})
		}
		if cfg.DreamAgent.Fallback != nil {
			dreamAgentClient.WithFallback(cfg.DreamAgent.Fallback.Model, cfg.DreamAgent.Fallback.Temperature, cfg.DreamAgent.Fallback.MaxTokens)
		}
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "dream_agent", Status: "ready", Detail: cfg.DreamAgent.Model})
	} else if memoryAgentClient != nil {
		dreamAgentClient = memoryAgentClient
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "dream_agent", Status: "ready", Detail: "fallback → " + cfg.MemoryAgent.Model})
	} else {
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "dream_agent", Status: "skipped"})
	}

	// --- Introspection agent client (optional) ---
	// Self-reflection agent — reviews each turn for identity observations.
	// Falls back to the memory agent client if introspection_agent.model
	// isn't configured.
	var introspectionClient *llm.Client
	if cfg.IntrospectionAgent.Model != "" {
		introspectionClient = llm.NewClient(cfg.OpenRouter.BaseURL, cfg.OpenRouter.APIKey, cfg.IntrospectionAgent.Model, cfg.IntrospectionAgent.Temperature, cfg.IntrospectionAgent.MaxTokens)
		timeout := cfg.IntrospectionAgent.Timeout
		if timeout == 0 {
			timeout = 60
		}
		introspectionClient.WithTimeout(time.Duration(timeout) * time.Second)
		if cfg.IntrospectionAgent.Provider != nil {
			introspectionClient.WithProvider(&llm.ProviderRouting{
				Order: cfg.IntrospectionAgent.Provider.Order,
				Only:  cfg.IntrospectionAgent.Provider.Only,
				Sort:  cfg.IntrospectionAgent.Provider.Sort,
			})
		}
		if cfg.IntrospectionAgent.Fallback != nil {
			introspectionClient.WithFallback(cfg.IntrospectionAgent.Fallback.Model, cfg.IntrospectionAgent.Fallback.Temperature, cfg.IntrospectionAgent.Fallback.MaxTokens)
		}
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "introspection", Status: "ready", Detail: cfg.IntrospectionAgent.Model})
	} else if memoryAgentClient != nil {
		introspectionClient = memoryAgentClient
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "introspection", Status: "ready", Detail: "fallback → " + cfg.MemoryAgent.Model})
	} else {
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "introspection", Status: "skipped"})
	}

	// --- Gateway (multi-adapter transport layer) ---
	// When gateway.adapters is configured, start enabled adapters (Gradio, etc.)
	// alongside the Telegram bot. The gateway handles non-Telegram transports;
	// Telegram stays on the legacy path for now (Phase 2 migrates it).

	// --- Gateway (multi-adapter transport layer) ---
	// When gateway.adapters is configured, the gateway manages ALL
	// adapters — including Telegram. The legacy bot.New() path only
	// runs when there's no gateway config at all (backwards compat).

	var gw *gateway.Gateway
	var tgBot *bot.Bot
	gwCtx, gwCancel := context.WithCancel(context.Background())
	defer gwCancel()
	gwDone := make(chan struct{})

	// Check if the gateway config includes a Telegram adapter.
	gatewayOwnsTelegram := false
	for _, a := range cfg.Gateway.Adapters {
		if a.Type == "telegram" && a.IsEnabled() {
			if adapterFilter == "" || adapterFilter == "telegram" {
				gatewayOwnsTelegram = true
			}
		}
	}

	// --- Project root ---
	// Used by the worker agent (task registry, reports/) and the scheduler
	// (task.yaml paths). Resolved early so both can reference it.
	rootDir, err := os.Getwd()
	if err != nil {
		log.Error("getting project root", "err", err)
		rootDir = "."
	}

	// --- Worker agent LLM clients ---
	// Build one LLM client per configured tier (low/medium/high).
	// Constructed before the gateway so the WorkerCallback can be passed
	// into gateway.Deps for adapters that create their own bot.Bot.
	workerLLMs := map[string]*llm.Client{}
	for tier, tcfg := range cfg.WorkerAgent.Tiers {
		if tcfg.Model == "" {
			continue
		}
		c := llm.NewClient(cfg.OpenRouter.BaseURL, cfg.OpenRouter.APIKey, tcfg.Model, tcfg.Temperature, tcfg.MaxTokens)
		timeout := tcfg.Timeout
		if timeout <= 0 {
			timeout = 120
		}
		c.WithTimeout(time.Duration(timeout) * time.Second)
		if tcfg.Provider != nil {
			c.WithProvider(&llm.ProviderRouting{
				Order: tcfg.Provider.Order,
				Only:  tcfg.Provider.Only,
				Sort:  tcfg.Provider.Sort,
			})
		}
		if tcfg.Fallback != nil {
			c.WithFallback(tcfg.Fallback.Model, tcfg.Fallback.Temperature, tcfg.Fallback.MaxTokens)
		}
		workerLLMs[tier] = c
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "worker_" + tier, Status: "ready", Detail: tcfg.Model})
	}
	if len(workerLLMs) > 0 {
		if err := workeragent.Init(rootDir); err != nil {
			log.Error("worker task registry failed", "err", err)
		}
	}

	// Build a shared WorkerCallback closure for both gateway and legacy bot paths.
	var workerCallbackFn func(taskType, note string)
	if len(workerLLMs) > 0 {
		reportsDir := filepath.Join(rootDir, "reports")
		if cfg.WorkerAgent.ReportsDir != "" {
			reportsDir = filepath.Join(rootDir, cfg.WorkerAgent.ReportsDir)
		}
		workerCallbackFn = func(taskType, note string) {
			tt := workeragent.Lookup(taskType)
			if tt == nil {
				log.Error("worker callback: unknown task type", "type", taskType)
				return
			}
			llmClient := workerLLMs[tt.ModelTier]
			if llmClient == nil {
				log.Error("worker callback: no LLM for tier", "tier", tt.ModelTier)
				return
			}
			go func() {
				result := workeragent.RunWorker(workeragent.WorkerInput{
					TaskType:    taskType,
					Instruction: note,
				}, workeragent.WorkerParams{
					LLM:          llmClient,
					TavilyClient: tavilyClient,
					Store:        store,
					Cfg:          cfg,
					ReportsDir:   reportsDir,
					EventBus:     bus,
				})

				if cfg.WorkerAgent.TelegraphToken != "" && result.ReportPath != "" {
					tc := telegraph.NewClient(cfg.WorkerAgent.TelegraphToken, cfg.Identity.Her)
					content, err := os.ReadFile(result.ReportPath)
					if err == nil {
						url, pubErr := tc.CreatePage(result.Title, string(content))
						if pubErr != nil {
							log.Warn("telegraph publish failed", "err", pubErr)
						} else {
							result.TelegraphURL = url
						}
					}
				}

				// Emit event — find the bot to get the event channel.
				// This is set after the bot is created, so we capture
				// tgBot by reference (it's always non-nil by call time).
				if tgBot != nil {
					ch := tgBot.AgentEventChannel()
					evt := agent.AgentEvent{
						Type:      agent.EventWorkerComplete,
						TaskName:  taskType,
						Summary:   result.Summary,
						ReportURL: result.TelegraphURL,
						Timestamp: time.Now(),
					}
					select {
					case ch <- evt:
					default:
						log.Warn("agent event channel full, dropping worker event")
					}
				}
			}()
		}
	}

	if len(cfg.Gateway.Adapters) > 0 {
		gwDeps := gateway.Deps{
			ChatLLM:          llmClient,
			DriverLLM:        driverClient,
			MemoryAgentLLM:   memoryAgentClient,
			MoodAgentLLM:     moodAgentClient,
			VisionLLM:        visionClient,
			ClassifierLLM:    classifierClient,
			DreamAgentLLM:    dreamAgentClient,
			IntrospectionLLM: introspectionClient,
			EmbedClient:      embedClient,
			TavilyClient:     tavilyClient,
			VoiceClient:      voiceClient,
			TTSClient:        ttsClient,
			ConfigPath:       cfgFile,
			WorkerCallback:   workerCallbackFn,
		}
		gw = gateway.New(cfg, gwDeps, bus)
		gw.AdapterFilter = adapterFilter
		go func() {
			defer close(gwDone)
			if err := gw.Run(gwCtx); err != nil && gwCtx.Err() == nil {
				log.Error("gateway exited with error", "err", err)
			}
		}()
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "gateway", Status: "ready"})
	} else {
		close(gwDone)
	}

	// --- Telegram bot (legacy path) ---
	// Only used when the gateway config does NOT include a Telegram adapter.
	// When the gateway owns Telegram, the TelegramAdapter creates its own
	// bot.Bot internally — no need for a second one here.
	if !skipTelegram && !gatewayOwnsTelegram {
		var botErr error
		tgBot, botErr = bot.New(cfg, cfgFile, llmClient, driverClient, memoryAgentClient, moodAgentClient, visionClient, classifierClient, dreamAgentClient, introspectionClient, embedClient, tavilyClient, voiceClient, ttsClient, store, bus)
		if botErr != nil {
			log.Error("Failed to create Telegram bot", "err", botErr)
			bus.Close()
			return
		}
		tgBot.SetOwnerChat(cfg.Telegram.OwnerChat)
		if workerCallbackFn != nil {
			tgBot.SetWorkerCallback(workerCallbackFn)
		}
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "telegram", Status: "ready"})
	} else if skipTelegram {
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "telegram", Status: "skipped"})
	}
	// When gatewayOwnsTelegram, the TelegramAdapter emitted its own
	// startup event inside bot.New().

	// --- Scheduler ---
	// Powers the extension-based task system (mood daily rollup, future
	// reminders, etc.). Loads every registered scheduler.Handler's
	// task.yaml at startup and dispatches on a 30s ticker. Extensions
	// self-register via init(); the scheduler just orchestrates.
	schedCtx, schedCancel := context.WithCancel(context.Background())
	schedDone := make(chan struct{})
	// Resolve the Telegram bot for scheduler wiring. When the gateway
	// owns Telegram, wait for it to be ready and grab the bot from there.
	var sendFunc func(int64, string) (int, error)
	if tgBot != nil {
		sendFunc = tgBot.SendWithID
	} else if gatewayOwnsTelegram && gw != nil {
		<-gw.Ready
		if gwBot := gw.TelegramBot(); gwBot != nil {
			tgBot = gwBot
			sendFunc = gwBot.SendWithID
		}
	}
	// Wire the agent event channel so worker handlers can emit completion
	// events that wake the driver agent.
	var agentEventCh chan<- agent.AgentEvent
	if tgBot != nil {
		agentEventCh = tgBot.AgentEventChannel()
	}

	schedDeps := &scheduler.Deps{
		Store:        store,
		Send:         sendFunc,
		ChatID:       cfg.Telegram.OwnerChat,
		WorkerLLMs:   workerLLMs,
		TavilyClient: tavilyClient,
		Cfg:          cfg,
		RootDir:      rootDir,
		AgentEventCh: agentEventCh,
		ScheduledPromptFn: func(prompt string) error {
			if agentEventCh == nil {
				return fmt.Errorf("send_prompt: no agent event channel")
			}
			evt := agent.AgentEvent{
				Type:     agent.EventSchedulerFired,
				Prompt:   prompt,
				TaskName: "send_prompt",
			}
			select {
			case agentEventCh <- evt:
				return nil
			default:
				return fmt.Errorf("send_prompt: agent event channel full")
			}
		},
	}
	sched, err := scheduler.New(store, schedDeps, rootDir)
	if err != nil {
		log.Error("scheduler startup failed; daily rollup disabled", "err", err)
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "scheduler", Status: "failed", Detail: err.Error()})
		close(schedDone) // nothing to wait for
	} else {
		go func() {
			defer close(schedDone)
			if err := sched.Run(schedCtx); err != nil {
				log.Error("scheduler stopped", "err", err)
			}
		}()
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "scheduler", Status: "ready"})
	}

	// --- Persona agent client (optional) ---
	// Dedicated model for the dreamer system (nightly reflections + persona rewrites).
	// Falls back to the memory agent client if persona_agent.model isn't configured —
	// same model, decoupled so they can diverge later.
	var personaAgentClient *llm.Client
	if cfg.PersonaAgent.Model != "" {
		personaAgentClient = llm.NewClient(cfg.OpenRouter.BaseURL, cfg.OpenRouter.APIKey, cfg.PersonaAgent.Model, cfg.PersonaAgent.Temperature, cfg.PersonaAgent.MaxTokens)
		if cfg.PersonaAgent.Timeout > 0 {
			personaAgentClient.WithTimeout(time.Duration(cfg.PersonaAgent.Timeout) * time.Second)
		}
		if cfg.PersonaAgent.Provider != nil {
			personaAgentClient.WithProvider(&llm.ProviderRouting{
				Order: cfg.PersonaAgent.Provider.Order,
				Only:  cfg.PersonaAgent.Provider.Only,
				Sort:  cfg.PersonaAgent.Provider.Sort,
			})
		}
		if cfg.PersonaAgent.Fallback != nil {
			personaAgentClient.WithFallback(cfg.PersonaAgent.Fallback.Model, cfg.PersonaAgent.Fallback.Temperature, cfg.PersonaAgent.Fallback.MaxTokens)
		}
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "persona_agent", Status: "ready", Detail: cfg.PersonaAgent.Model})
	} else if memoryAgentClient != nil {
		personaAgentClient = memoryAgentClient
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "persona_agent", Status: "ready", Detail: "fallback → " + cfg.MemoryAgent.Model})
	} else {
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "persona_agent", Status: "skipped"})
	}

	// --- Dreamer ---
	// The dreamer goroutine runs nightly reflection and gated persona rewrites.
	// Uses the dedicated persona agent client (or memory agent fallback).
	// Validate persona files exist before starting the dreamer.
	for _, pf := range []string{cfg.Persona.PromptFile, cfg.Persona.PersonaFile, cfg.Persona.AgentPromptFile} {
		if pf != "" {
			if _, err := os.Stat(pf); err != nil {
				log.Warn("persona file missing — dreamer may fail", "path", pf, "err", err)
			}
		}
	}

	dreamerCtx, dreamerCancel := context.WithCancel(context.Background())
	dreamerDone := make(chan struct{})
	if personaAgentClient != nil {
		dreamHour := cfg.Persona.DreamHour
		if dreamHour == 0 {
			dreamHour = 4
		}
		minDays := cfg.Persona.MinRewriteDays
		if minDays == 0 {
			minDays = 7
		}
		minRefl := cfg.Persona.MinReflections
		if minRefl == 0 {
			minRefl = 3
		}
		go func() {
			defer close(dreamerDone)
			persona.StartDreamer(dreamerCtx, persona.DreamerParams{
				LLM:           personaAgentClient,
				DreamLLM:      dreamAgentClient,
				ClassifierLLM: classifierClient,
				Embed:         embedClient,
				Store:         store,
				Cfg:           cfg,
				EventBus:      bus,
				DreamHour:     dreamHour,
				MinDays:       minDays,
				MinRefl:       minRefl,
			})
		}()
	} else {
		log.Warn("dreamer disabled — no persona or memory agent model configured")
		close(dreamerDone) // nothing to wait for
	}

	// --- KV Sync Poller (prod only) ---
	// When a dev session ends, the dev machine writes `dev_session_ended`
	// to KV. This poller detects that signal and triggers a Pull to sync
	// any data the dev session created. Only runs in prod mode (not
	// webhook/dev) and only when D1 sync is active.
	var kvPollerCancel context.CancelFunc
	if synced, ok := store.(*memory.SyncedStore); ok && cfg.Cloudflare.KVNamespaceID != "" && cfg.Telegram.Mode != "webhook" {
		var kvPollerCtx context.Context
		kvPollerCtx, kvPollerCancel = context.WithCancel(context.Background())
		kv := &kvClient{
			accountID:   cfg.Cloudflare.AccountID,
			apiToken:    cfg.Cloudflare.APIToken,
			namespaceID: cfg.Cloudflare.KVNamespaceID,
			http:        &http.Client{Timeout: kvClientTimeout},
		}
		pollerInterval := time.Duration(cfg.Cloudflare.Sync.PollerInterval) * time.Second
		if pollerInterval == 0 {
			pollerInterval = 30 * time.Second
		}
		go startSyncPoller(kvPollerCtx, kv, synced, pollerInterval)
		log.Info("d1 sync poller started — watching for dev session end")
	}

	// --- Signal handling + bot start ---
	// Listen for SIGINT/SIGTERM. When received, shut everything down
	// and close the bus (which makes the TUI exit).

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start the bot in yet another goroutine — tgBot.Start() blocks.
	// When the gateway owns Telegram, the adapter already called Start()
	// in its own goroutine — don't double-start.
	botDone := make(chan struct{})
	if tgBot != nil && !gatewayOwnsTelegram {
		go func() {
			tgBot.Start()
			close(botDone)
		}()
	} else {
		close(botDone)
	}

	// Wait for either: signal received OR TUI quit (user pressed q)
	select {
	case sig := <-sigChan:
		log.Info("Signal received, shutting down", "signal", sig)
	case <-quitCh:
		log.Info("TUI quit requested, shutting down")
	}

	// --- Cleanup ---

	// Dev mode cleanup (clear KV routing keys) runs first — we want
	// traffic to route back to prod ASAP, before the bot actually stops.
	if devCleanup != nil {
		devCleanup()
	}

	gwCancel()      // stop gateway adapters
	dreamerCancel() // tell the dreamer goroutine to stop at its next wake-up
	schedCancel()   // same for the scheduler runner
	if kvPollerCancel != nil {
		kvPollerCancel()
	}

	// Wait for background goroutines to finish (up to 10s). This prevents
	// mid-transaction database corruption from interrupted writes.
	shutdownTimeout := time.After(10 * time.Second)
	for _, ch := range []chan struct{}{gwDone, dreamerDone, schedDone} {
		select {
		case <-ch:
		case <-shutdownTimeout:
			log.Warn("shutdown timeout waiting for background goroutines")
		}
	}

	for _, sc := range []struct {
		name string
		cmd  *exec.Cmd
	}{
		{"parakeet-server", sttProcess},
		{"piper TTS server", ttsProcess},
		{"embed sidecar", embedProcess},
	} {
		if sc.cmd != nil && sc.cmd.Process != nil {
			log.Info("stopping "+sc.name, "pid", sc.cmd.Process.Pid)
			_ = syscall.Kill(-sc.cmd.Process.Pid, syscall.SIGTERM)
			done := make(chan struct{})
			go func() { sc.cmd.Process.Wait(); close(done) }()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				_ = syscall.Kill(-sc.cmd.Process.Pid, syscall.SIGKILL)
				sc.cmd.Process.Wait()
			}
		}
	}
	if tgBot != nil && !gatewayOwnsTelegram {
		tgBot.Stop()
	}
	<-botDone // wait for tgBot.Start() to return

	bus.Close() // closes event channels → TUI exits
}

// runBotPlain runs the bot without a TUI (piped output, CI, etc.).
// Same logic as the original runBot but with events going to file logger only.
func runBotPlain(cfg *config.Config, store memory.Store, bus *tui.Bus) error {
	// In plain mode, also write events to stderr so they're visible
	tui.StartFileLogger(bus, os.Stderr)

	quitCh := make(chan struct{})
	runBotBackground(cfg, store, bus, nil, quitCh)
	return nil
}

// --- Sidecar helpers ---

// killStaleProcess finds and kills any process listening on the given port.
// This cleans up orphaned sidecars from previous runs that didn't shut down
// cleanly (force-quit, crash, terminal closed). Uses lsof on macOS.
//
// This is like Python's "check if port is in use before binding" pattern,
// but more aggressive — we kill the squatter rather than failing.
func killStaleProcess(port string) {
	// lsof -ti :PORT returns just the PID(s) listening on that port
	out, err := exec.Command("lsof", "-ti", ":"+port).Output()
	if err != nil || len(out) == 0 {
		return // no stale process
	}

	// May return multiple PIDs (one per line)
	pids := strings.TrimSpace(string(out))
	for _, pidStr := range strings.Split(pids, "\n") {
		pidStr = strings.TrimSpace(pidStr)
		if pidStr == "" {
			continue
		}
		pid, err := strconv.Atoi(pidStr)
		if err != nil {
			continue
		}
		log.Warn("killing stale process on port "+port, "pid", pid)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}

	// Brief pause to let the OS release the port
	time.Sleep(200 * time.Millisecond)
}

// killStaleSelf reads her.pid and kills any previous her run instance that
// didn't exit cleanly. Same idea as killStaleProcess, but using a PID file
// instead of a port scan — her doesn't bind a TCP port so lsof can't find it.
//
// This prevents the "two bots competing for the same Telegram token" problem
// where messages go to the old instance and the new TUI sees nothing.
func killStaleSelf(pidFile string) {
	data, err := os.ReadFile(pidFile)
	if err != nil {
		return // no PID file — clean slate
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		_ = os.Remove(pidFile)
		return
	}

	// Signal 0 checks existence without actually sending a signal.
	// On Unix, FindProcess always succeeds — the signal is the real check.
	if err := syscall.Kill(pid, 0); err != nil {
		// Process is gone — just clean up the file.
		_ = os.Remove(pidFile)
		return
	}

	log.Warn("killing stale her process from previous run", "pid", pid)
	_ = syscall.Kill(pid, syscall.SIGTERM)
	time.Sleep(300 * time.Millisecond)
	// Force-kill if SIGTERM wasn't enough (e.g., process was stuck in I/O).
	_ = syscall.Kill(pid, syscall.SIGKILL)
	_ = os.Remove(pidFile)
}

// stopManagedServiceIfRunning checks if a supervised service (`her start`)
// is active and stops it before a foreground `her run`. Without this, two
// instances race for the same Telegram token and PID file, causing one to
// SIGTERM the other mid-startup.
func stopManagedServiceIfRunning(cfg *config.Config) {
	botName := cfg.Identity.Her
	if botName == "" {
		return
	}
	label := procmgr.EffectiveLabel(cfg.Update.ServiceLabel, botName)
	mgr, err := procmgr.New(label)
	if err != nil {
		return
	}

	if !mgr.IsManaged() {
		return // not running under a supervisor — nothing to do
	}

	log.Info("stopping managed service before foreground run", "supervisor", mgr.Name(), "label", label)
	if err := mgr.Stop(); err != nil {
		log.Warn("could not stop managed service", "err", err)
	}
	// Give the old process a moment to release the Telegram token.
	time.Sleep(500 * time.Millisecond)
}

// startSTTSidecar launches the parakeet-server sidecar for local STT.
// Skipped entirely for remote engines (e.g. "whisper") — those talk to an
// external API and don't need a local process.
// Output goes to sidecarOut (log file in TUI mode, stderr in plain mode).
func startSTTSidecar(cfg *config.Config, bus *tui.Bus, voiceClient *voice.Client) *exec.Cmd {
	if cfg.Voice.STT.Engine != config.STTEngineParakeet {
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "stt", Status: "ready", Detail: cfg.Voice.STT.Engine + " (remote)"})
		return nil
	}

	sttPath, err := exec.LookPath("parakeet-server")
	if err != nil {
		log.Warn("parakeet-server not found in PATH — voice memos will fail. Run: her setup")
		return nil
	}

	sttHost := "127.0.0.1"
	sttPort := "8765"
	if u, err := url.Parse(cfg.Voice.STT.BaseURL); err == nil {
		if h := u.Hostname(); h != "" {
			sttHost = h
		}
		if p := u.Port(); p != "" {
			sttPort = p
		}
	}

	// Kill any stale parakeet-server from a previous run that didn't clean up.
	killStaleProcess(sttPort)

	sttProcess := exec.Command(sttPath, "-m", cfg.Voice.STT.Model, "-h", sttHost, "-p", sttPort)
	// Send sidecar output to the log file (TUI mode) or stderr (plain mode).
	// No pipes — pipes + Setsid caused the sidecars to malfunction.
	// The important metrics (transcription time, etc.) flow through our
	// Go logger bridge in voice/stt.go anyway.
	sttProcess.Stdout = sidecarOut
	sttProcess.Stderr = sidecarOut
	sttProcess.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := sttProcess.Start(); err != nil {
		log.Error("failed to start parakeet-server", "err", err)
		return nil
	}

	bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "stt", Status: "ready", Detail: "pid=" + fmt.Sprint(sttProcess.Process.Pid)})

	go func() {
		time.Sleep(3 * time.Second)
		if voiceClient.IsAvailable() {
			log.Info("parakeet-server is ready")
		} else {
			log.Warn("parakeet-server not responding yet — first voice memo may be slow")
		}
	}()

	return sttProcess
}

// startTTSSidecar launches the Piper TTS server.
// Output goes to sidecarOut (log file in TUI mode, stderr in plain mode).
func startTTSSidecar(cfg *config.Config, bus *tui.Bus, ttsClient *voice.TTSClient) *exec.Cmd {
	uvPath, err := exec.LookPath("uv")
	if err != nil {
		log.Warn("uv not found in PATH — TTS will fail. Run: her setup")
		return nil
	}

	ttsHost := "127.0.0.1"
	ttsPort := "8766"
	if u, err := url.Parse(cfg.Voice.TTS.BaseURL); err == nil {
		if h := u.Hostname(); h != "" {
			ttsHost = h
		}
		if p := u.Port(); p != "" {
			ttsPort = p
		}
	}

	// Kill any stale TTS server from a previous run.
	killStaleProcess(ttsPort)

	ttsScript := filepath.Join("scripts", "tts_server.py")
	ttsArgs := []string{"run", ttsScript, "--host", ttsHost, "--port", ttsPort}

	// Pass pause config from config.yaml so the sidecar doesn't hardcode values.
	p := cfg.Voice.TTS.Pauses
	if p.Paragraph > 0 {
		ttsArgs = append(ttsArgs, "--pause-paragraph", strconv.Itoa(p.Paragraph))
	}
	if p.Line > 0 {
		ttsArgs = append(ttsArgs, "--pause-line", strconv.Itoa(p.Line))
	}
	if p.Sentence > 0 {
		ttsArgs = append(ttsArgs, "--pause-sentence", strconv.Itoa(p.Sentence))
	}
	if p.Comma > 0 {
		ttsArgs = append(ttsArgs, "--pause-comma", strconv.Itoa(p.Comma))
	}
	if p.Semi > 0 {
		ttsArgs = append(ttsArgs, "--pause-semi", strconv.Itoa(p.Semi))
	}

	ttsProcess := exec.Command(uvPath, ttsArgs...)
	ttsProcess.Stdout = sidecarOut
	ttsProcess.Stderr = sidecarOut
	ttsProcess.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := ttsProcess.Start(); err != nil {
		log.Error("failed to start TTS server", "err", err)
		return nil
	}

	bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "tts", Status: "ready", Detail: "pid=" + fmt.Sprint(ttsProcess.Process.Pid)})

	go func() {
		time.Sleep(5 * time.Second)
		if ttsClient.IsAvailable() {
			log.Info("piper TTS server is ready")
		} else {
			log.Warn("piper TTS server not responding yet — first voice reply may be slow")
		}
	}()

	return ttsProcess
}

// startEmbedSidecar launches a user-configured embedding server command.
// Unlike the STT/TTS sidecars whose commands are hardcoded, embed is flexible —
// start_command could be "lms load <model>", "ollama serve", or anything else.
// Because of this flexibility we skip killStaleProcess: a command like
// "lms load" operates on an already-running server process, and killing the
// port would take down the entire LM Studio app.
func startEmbedSidecar(cfg *config.Config, bus *tui.Bus, embedClient *embed.Client) *exec.Cmd {
	// Split "lms load nomic-embed-text-v1.5" → ["lms", "load", "nomic-embed-text-v1.5"]
	// strings.Fields handles any amount of whitespace between tokens.
	parts := strings.Fields(cfg.Embed.StartCommand)
	if len(parts) == 0 {
		return nil
	}

	cmdPath, err := exec.LookPath(parts[0])
	if err != nil {
		log.Warn("embed start_command not found in PATH", "cmd", parts[0])
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "embed", Status: "failed", Detail: "command not found: " + parts[0]})
		return nil
	}

	embedProcess := exec.Command(cmdPath, parts[1:]...)
	embedProcess.Stdout = sidecarOut
	embedProcess.Stderr = sidecarOut
	// Setpgid: true puts the child in its own process group so we can
	// SIGKILL the whole group on shutdown (negative PID kills the group).
	embedProcess.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := embedProcess.Start(); err != nil {
		log.Error("failed to start embed sidecar", "err", err)
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "embed", Status: "failed", Detail: err.Error()})
		return nil
	}

	bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "embed", Status: "starting", Detail: "pid=" + fmt.Sprint(embedProcess.Process.Pid)})

	// Poll until the server is ready or 30 seconds have elapsed.
	// Runs in the background — never blocks the user from continuing startup.
	go func() {
		deadline := time.Now().Add(30 * time.Second)
		for time.Now().Before(deadline) {
			time.Sleep(1 * time.Second)
			if embedClient.IsAvailable() {
				log.Info("embed server is ready")
				bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "embed", Status: "ready", Detail: cfg.Embed.Model})
				return
			}
		}
		log.Warn("embed server did not respond within 30s — semantic recall degraded to keyword fallback")
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "embed", Status: "failed", Detail: "timeout"})
	}()

	return embedProcess
}

// startSyncPoller checks KV every 30 seconds for a `dev_session_ended`
// signal. When the dev machine (MacBook) stops a dev session, it writes
// this key to KV. The prod machine (Mac Mini) detects it here, pulls
// fresh data from D1, and deletes the key.
//
// This is the glue between dev shutdown and prod sync — without it,
// prod wouldn't know to pull until its next restart. Think of it like
// a very simple message queue over KV: one producer, one consumer,
// one message at a time.
func startSyncPoller(ctx context.Context, kv *kvClient, synced *memory.SyncedStore, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			val, err := kv.get("dev_session_ended")
			if err != nil {
				log.Warn("sync poller: KV read failed", "err", err)
				continue
			}
			if val == "" {
				continue // no signal — dev session still active or not started
			}

			log.Info("dev session ended — pulling from D1", "ended_at", val)
			pullCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
			if err := synced.Pull(pullCtx); err != nil {
				log.Error("sync poller: pull failed", "err", err)
			}
			cancel()

			// Clear the signal so we don't pull again next cycle.
			if err := kv.delete("dev_session_ended"); err != nil {
				log.Warn("sync poller: failed to clear dev_session_ended key", "err", err)
			}
		}
	}
}
