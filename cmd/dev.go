package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"her/bot"
	"her/config"
	"her/d1"
	"her/embed"
	"her/llm"
	"her/logger"
	"her/memory"
	"her/retry"
	"her/search"
	"her/tui"

	"github.com/spf13/cobra"
	"gopkg.in/natefinch/lumberjack.v2"
)

var devLog = logger.WithPrefix("dev")

var devCmd = &cobra.Command{
	Use:   "dev",
	Short: "Start the dev server with HTTP API (no Telegram)",
	Long: `Starts the full agent pipeline with a local HTTP API on :7777.
Use with the Gradio dev chat frontend (scripts/dev_chat.py).
No Telegram token required — the VPS owns Telegram, your Mac owns dev mode.

  Terminal 1:  her dev
  Terminal 2:  uv run scripts/dev_chat.py
  Browser:     http://localhost:7860`,
	RunE: runDev,
}

func init() {
	rootCmd.AddCommand(devCmd)
}

// chatRequest is the JSON body for POST /api/chat.
type chatRequest struct {
	Message        string `json:"message"`
	ConversationID string `json:"conversation_id"`
}

// chatResponse is the JSON response from POST /api/chat.
type chatResponse struct {
	Reply          string `json:"reply"`
	ConversationID string `json:"conversation_id"`
}

func runDev(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		devLog.Fatal("failed to load config", "err", err)
	}
	cfg.ExportEnv()

	if cfg.OpenRouter.APIKey == "" {
		devLog.Fatal("LLM API key is required — set OPENROUTER_API_KEY or fill config.yaml")
	}

	// --- Store ---
	store, err := memory.NewStore(cfg.Memory.DBPath, cfg.Embed.Dimension)
	if err != nil {
		devLog.Fatal("failed to initialize database", "err", err)
	}
	store.AutoLinkCount = cfg.Memory.AutoLinkCount
	store.AutoLinkThreshold = cfg.Memory.AutoLinkThreshold

	var botStore memory.Store = store
	if cfg.Cloudflare.D1DatabaseID != "" {
		d1Client := d1.NewClient(cfg.Cloudflare.AccountID, cfg.Cloudflare.D1DatabaseID, cfg.Cloudflare.APIToken)
		if d1Client != nil {
			synced, err := memory.NewSyncedStore(store, d1Client)
			if err != nil {
				devLog.Fatal("failed to initialize D1 sync", "err", err)
			}
			if cfg.Cloudflare.Sync.BatchSize > 0 {
				synced.BatchSize = cfg.Cloudflare.Sync.BatchSize
			}
			if cfg.Cloudflare.Sync.PullPageSize > 0 {
				synced.PullPageSize = cfg.Cloudflare.Sync.PullPageSize
			}

			pullCtx, pullCancel := context.WithTimeout(context.Background(), 60*time.Second)
			err = retry.Do(pullCtx, retry.Config{
				MaxAttempts: 3,
				Backoff:     retry.Exponential,
				InitialWait: 2 * time.Second,
			}, func() error {
				return synced.Pull(pullCtx)
			})
			if err != nil {
				devLog.Error("d1 pull failed — continuing with local data", "err", err)
			}
			pullCancel()

			botStore = synced
			devLog.Info("d1 sync enabled")
		}
	}
	defer botStore.Close()

	// --- Event bus + logging ---
	bus := tui.NewBus()

	logFile := &lumberjack.Logger{
		Filename:   "logs/dev.log",
		MaxSize:    10,
		MaxBackups: 0,
		MaxAge:     0,
		LocalTime:  true,
	}
	_ = logFile.Rotate()
	logger.Init(bus, nil)
	tui.StartFileLogger(bus, logFile)

	devLog.Info("SESSION_START",
		"mode", "dev",
		"driver_model", cfg.Driver.Model,
		"chat_model", cfg.Chat.Model,
	)

	// --- LLM clients ---
	// Same initialization as cmd/run.go — each agent gets its own client
	// with model, temperature, timeout, fallback, and provider routing
	// from config.yaml.

	llmClient := llm.NewClient(cfg.OpenRouter.BaseURL, cfg.OpenRouter.APIKey, cfg.Chat.Model, cfg.Chat.Temperature, cfg.Chat.MaxTokens)
	if cfg.Chat.Timeout > 0 {
		llmClient.WithTimeout(time.Duration(cfg.Chat.Timeout) * time.Second)
	}
	if cfg.Chat.Provider != nil {
		llmClient.WithProvider(&llm.ProviderRouting{Order: cfg.Chat.Provider.Order, Only: cfg.Chat.Provider.Only, Sort: cfg.Chat.Provider.Sort})
	}
	if cfg.Chat.Fallback != nil {
		llmClient.WithFallback(cfg.Chat.Fallback.Model, cfg.Chat.Fallback.Temperature, cfg.Chat.Fallback.MaxTokens)
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
	}

	var classifierClient *llm.Client
	if cfg.Classifier.Model != "" {
		classifierClient = llm.NewClient(cfg.OpenRouter.BaseURL, cfg.OpenRouter.APIKey, cfg.Classifier.Model, cfg.Classifier.Temperature, cfg.Classifier.MaxTokens)
	}

	var memoryAgentClient *llm.Client
	if cfg.MemoryAgent.Model != "" {
		memoryAgentClient = llm.NewClient(cfg.OpenRouter.BaseURL, cfg.OpenRouter.APIKey, cfg.MemoryAgent.Model, cfg.MemoryAgent.Temperature, cfg.MemoryAgent.MaxTokens)
		if cfg.MemoryAgent.Timeout > 0 {
			memoryAgentClient.WithTimeout(time.Duration(cfg.MemoryAgent.Timeout) * time.Second)
		}
		if cfg.MemoryAgent.Provider != nil {
			memoryAgentClient.WithProvider(&llm.ProviderRouting{Order: cfg.MemoryAgent.Provider.Order, Only: cfg.MemoryAgent.Provider.Only, Sort: cfg.MemoryAgent.Provider.Sort})
		}
		if cfg.MemoryAgent.Fallback != nil {
			memoryAgentClient.WithFallback(cfg.MemoryAgent.Fallback.Model, cfg.MemoryAgent.Fallback.Temperature, cfg.MemoryAgent.Fallback.MaxTokens)
		}
		if cfg.MemoryAgent.Reasoning != nil && cfg.MemoryAgent.Reasoning.Enabled != nil {
			memoryAgentClient.WithReasoning(&llm.ReasoningControl{Enabled: cfg.MemoryAgent.Reasoning.Enabled})
		}
	}

	var moodAgentClient *llm.Client
	if cfg.MoodAgent.Model != "" {
		mTemp := cfg.MoodAgent.Temperature
		if mTemp == 0 {
			mTemp = 0.2
		}
		mTokens := cfg.MoodAgent.MaxTokens
		if mTokens == 0 {
			mTokens = 512
		}
		moodAgentClient = llm.NewClient(cfg.OpenRouter.BaseURL, cfg.OpenRouter.APIKey, cfg.MoodAgent.Model, mTemp, mTokens)
		if cfg.MoodAgent.Timeout > 0 {
			moodAgentClient.WithTimeout(time.Duration(cfg.MoodAgent.Timeout) * time.Second)
		}
		if cfg.MoodAgent.Provider != nil {
			moodAgentClient.WithProvider(&llm.ProviderRouting{Order: cfg.MoodAgent.Provider.Order, Only: cfg.MoodAgent.Provider.Only, Sort: cfg.MoodAgent.Provider.Sort})
		}
		if cfg.MoodAgent.Fallback != nil {
			moodAgentClient.WithFallback(cfg.MoodAgent.Fallback.Model, cfg.MoodAgent.Fallback.Temperature, cfg.MoodAgent.Fallback.MaxTokens)
		}
	}

	var embedClient *embed.Client
	if cfg.Embed.BaseURL != "" && cfg.Embed.Model != "" {
		embedClient = embed.NewClient(cfg.Embed.BaseURL, cfg.Embed.Model, cfg.Embed.APIKey, cfg.Embed.Dimension)
	}

	var embedProcess *exec.Cmd
	if embedClient != nil {
		if embedClient.IsAvailable() {
			devLog.Info("embed server ready", "model", cfg.Embed.Model)
		} else if cfg.Embed.StartCommand != "" {
			embedProcess = startEmbedSidecar(cfg, bus, embedClient)
		} else {
			devLog.Warn("embed server not responding — semantic recall degraded")
		}
	}

	var tavilyClient *search.TavilyClient
	if cfg.Search.TavilyAPIKey != "" {
		tavilyClient = search.NewTavilyClient(cfg.Search.TavilyAPIKey, cfg.Search.TavilyBaseURL)
	}

	// Dream + introspection agents — fall back to memory agent if not configured.
	var dreamAgentClient *llm.Client
	if cfg.DreamAgent.Model != "" {
		dreamAgentClient = llm.NewClient(cfg.OpenRouter.BaseURL, cfg.OpenRouter.APIKey, cfg.DreamAgent.Model, cfg.DreamAgent.Temperature, cfg.DreamAgent.MaxTokens)
		timeout := cfg.DreamAgent.Timeout
		if timeout == 0 {
			timeout = 120
		}
		dreamAgentClient.WithTimeout(time.Duration(timeout) * time.Second)
	} else if memoryAgentClient != nil {
		dreamAgentClient = memoryAgentClient
	}

	var introspectionClient *llm.Client
	if cfg.IntrospectionAgent.Model != "" {
		introspectionClient = llm.NewClient(cfg.OpenRouter.BaseURL, cfg.OpenRouter.APIKey, cfg.IntrospectionAgent.Model, cfg.IntrospectionAgent.Temperature, cfg.IntrospectionAgent.MaxTokens)
		timeout := cfg.IntrospectionAgent.Timeout
		if timeout == 0 {
			timeout = 60
		}
		introspectionClient.WithTimeout(time.Duration(timeout) * time.Second)
	} else if memoryAgentClient != nil {
		introspectionClient = memoryAgentClient
	}

	// --- Create bot (no Telegram) ---
	devBot, err := bot.NewDev(cfg, cfgFile, llmClient, driverClient, memoryAgentClient, moodAgentClient, visionClient, classifierClient, dreamAgentClient, introspectionClient, embedClient, tavilyClient, botStore, bus)
	if err != nil {
		devLog.Fatal("failed to create dev bot", "err", err)
	}

	// --- Conversation state ---
	var (
		currentConvID string
		convMu        sync.Mutex
	)

	getConvID := func() string {
		convMu.Lock()
		defer convMu.Unlock()
		if currentConvID == "" {
			currentConvID = fmt.Sprintf("dev-%d", time.Now().UnixNano())
		}
		return currentConvID
	}

	// --- HTTP handlers ---
	mux := http.NewServeMux()

	// CORS middleware — restrict to Gradio's default origin (localhost:7860).
	cors := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Access-Control-Allow-Origin", "http://localhost:7860")
			w.Header().Set("Access-Control-Allow-Methods", "POST, GET, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			if r.Method == "OPTIONS" {
				w.WriteHeader(http.StatusNoContent)
				return
			}
			h(w, r)
		}
	}

	mux.HandleFunc("/api/chat", cors(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}

		var req chatRequest
		r.Body = http.MaxBytesReader(w, r.Body, 1<<20) // 1MB limit
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, `{"error": "invalid JSON"}`, http.StatusBadRequest)
			return
		}
		if req.Message == "" {
			http.Error(w, `{"error": "message is required"}`, http.StatusBadRequest)
			return
		}

		convID := req.ConversationID
		if convID == "" {
			convID = getConvID()
		}

		fe := bot.NewDevFrontend()
		reply, err := devBot.ProcessMessage(fe, req.Message, convID)
		if err != nil {
			devLog.Error("agent error", "err", err)
			w.WriteHeader(http.StatusInternalServerError)
			json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
			return
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(chatResponse{
			Reply:          reply,
			ConversationID: convID,
		})
	}))

	mux.HandleFunc("/api/clear", cors(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		convMu.Lock()
		currentConvID = fmt.Sprintf("dev-%d", time.Now().UnixNano())
		newID := currentConvID
		convMu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{"conversation_id": newID})
	}))

	mux.HandleFunc("/api/status", cors(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]string{
			"status":       "ok",
			"mode":         "dev",
			"chat_model":   cfg.Chat.Model,
			"driver_model": cfg.Driver.Model,
		})
	}))

	// --- Start HTTP server ---
	srv := &http.Server{
		Addr:    ":7777",
		Handler: mux,
	}

	go func() {
		devLog.Info("dev server listening", "addr", "http://localhost:7777")
		devLog.Info("start Gradio with: uv run scripts/dev_chat.py")
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			devLog.Fatal("http server error", "err", err)
		}
	}()

	// --- Wait for signal ---
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh

	devLog.Info("shutting down...")

	shutCtx, shutCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutCancel()
	_ = srv.Shutdown(shutCtx)

	if embedProcess != nil && embedProcess.Process != nil {
		_ = syscall.Kill(-embedProcess.Process.Pid, syscall.SIGTERM)
		embedProcess.Process.Wait()
	}

	bus.Close()
	return nil
}
