package cmd

import (
	"os"
	"os/signal"
	"syscall"

	"her/bot"
	"her/config"
	"her/embed"
	"her/llm"
	"her/logger"
	"her/memory"
	"her/scheduler"
	"her/search"

	"github.com/spf13/cobra"
)

// log is the package-level logger for the cmd package.
var log = logger.WithPrefix("cmd")

var runCmd = &cobra.Command{
	Use:   "run",
	Short: "Start the bot process (foreground)",
	Long:  "Loads config, initializes the database and API clients, and runs the Telegram bot.\nThis blocks until the process receives SIGINT or SIGTERM.",
	RunE:  runBot,
}

func init() {
	rootCmd.AddCommand(runCmd)
}

// runBot contains all the initialization and startup logic that was
// previously in main(). Moved here so it can be invoked as "her run".
func runBot(cmd *cobra.Command, args []string) error {
	// Logger is already configured in the logger package (logger/logger.go).
	// All packages create sub-loggers via logger.With() which inherit
	// timestamps, level, and format settings from the shared base logger.

	// Load configuration from the file specified by --config.
	cfg, err := config.Load(cfgFile)
	if err != nil {
		log.Fatal("Failed to load config", "err", err)
	}

	// Quick sanity checks — fail fast if critical config is missing.
	if cfg.Telegram.Token == "" {
		log.Fatal("Telegram token is required — set TELEGRAM_BOT_TOKEN env var or fill in config.yaml")
	}
	if cfg.LLM.APIKey == "" {
		log.Fatal("LLM API key is required — set OPENROUTER_API_KEY env var or fill in config.yaml")
	}

	// Initialize the SQLite database.
	store, err := memory.NewStore(cfg.Memory.DBPath)
	if err != nil {
		log.Fatal("Failed to initialize database", "err", err)
	}
	defer store.Close()

	log.Info("Database initialized", "path", cfg.Memory.DBPath)

	// Create the LLM client (conversational model — Deepseek).
	llmClient := llm.NewClient(
		cfg.LLM.BaseURL,
		cfg.LLM.APIKey,
		cfg.LLM.Model,
		cfg.LLM.Temperature,
		cfg.LLM.MaxTokens,
	)
	log.Info("LLM client configured", "url", cfg.LLM.BaseURL, "model", cfg.LLM.Model)

	// Create the agent LLM client (tool-calling orchestrator).
	// This shares the same base URL and API key as the main client
	// but uses a different model optimized for tool calling.
	agentModel := cfg.Agent.Model
	if agentModel == "" {
		agentModel = "liquid/lfm-2.5-1.2b-instruct:free"
	}
	agentTemp := cfg.Agent.Temperature
	if agentTemp == 0 {
		agentTemp = 0.1
	}
	agentMaxTokens := cfg.Agent.MaxTokens
	if agentMaxTokens == 0 {
		agentMaxTokens = 512
	}
	agentClient := llm.NewClient(
		cfg.LLM.BaseURL,
		cfg.LLM.APIKey,
		agentModel,
		agentTemp,
		agentMaxTokens,
	)
	log.Info("Agent client configured", "url", cfg.LLM.BaseURL, "model", agentModel)

	// Create the vision LLM client (image understanding — Gemini 3 Flash).
	// Same pattern as the agent client: shares the base URL and API key
	// from the main LLM config, but uses its own model/temperature/max_tokens.
	// Optional — if no vision model is configured, image features are disabled.
	var visionClient *llm.Client
	if cfg.Vision.Model != "" {
		visionTemp := cfg.Vision.Temperature
		if visionTemp == 0 {
			visionTemp = 0.3
		}
		visionMaxTokens := cfg.Vision.MaxTokens
		if visionMaxTokens == 0 {
			visionMaxTokens = 512
		}
		visionClient = llm.NewClient(
			cfg.LLM.BaseURL,
			cfg.LLM.APIKey,
			cfg.Vision.Model,
			visionTemp,
			visionMaxTokens,
		)
		log.Info("Vision client configured", "model", cfg.Vision.Model)
	} else {
		log.Info("Vision client not configured — image understanding disabled")
	}

	// Create the embedding client for semantic similarity.
	// Optional — if not configured, the agent skips duplicate checking.
	var embedClient *embed.Client
	if cfg.Embed.BaseURL != "" && cfg.Embed.Model != "" {
		embedClient = embed.NewClient(cfg.Embed.BaseURL, cfg.Embed.Model)
		log.Info("Embed client configured", "url", cfg.Embed.BaseURL, "model", cfg.Embed.Model, "threshold", cfg.Embed.SimilarityThreshold)
	} else {
		log.Info("Embed client not configured — semantic duplicate checking disabled")
	}

	// Create the Tavily client for web search and URL extraction.
	// Optional — if not configured, the agent's search tools will
	// return an error message instead of crashing.
	var tavilyClient *search.TavilyClient
	if cfg.Search.TavilyAPIKey != "" {
		tavilyClient = search.NewTavilyClient(cfg.Search.TavilyAPIKey, cfg.Search.TavilyBaseURL)
		log.Info("Tavily client configured (web search enabled)")
	} else {
		log.Info("Tavily client not configured — web search disabled")
	}

	// Create and configure the Telegram bot.
	tgBot, err := bot.New(cfg, llmClient, agentClient, visionClient, embedClient, tavilyClient, store)
	if err != nil {
		log.Fatal("Failed to create Telegram bot", "err", err)
	}

	// Start the scheduler if owner_chat is configured.
	// The scheduler needs to know WHERE to send messages — that's the
	// owner's Telegram chat ID. Without it, reminders get created in
	// the DB but never delivered. The /status command shows your chat ID.
	var sched *scheduler.Scheduler
	if cfg.Telegram.OwnerChat != 0 {
		ownerChat := cfg.Telegram.OwnerChat
		sendFn := func(text string) error {
			return tgBot.SendToChat(ownerChat, text)
		}
		sched = scheduler.New(store, sendFn, cfg.Scheduler.Timezone)
		sched.Start()
	} else {
		log.Warn("scheduler disabled — set telegram.owner_chat in config.yaml (use /status to find your chat ID)")
	}

	// Handle graceful shutdown.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigChan
		log.Info("Signal received, shutting down", "signal", sig)
		if sched != nil {
			sched.Stop()
		}
		tgBot.Stop()
	}()

	// Start the bot. This blocks until Stop() is called.
	tgBot.Start()
	log.Info("Bot stopped. Goodbye!")

	return nil
}
