package cmd

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"her-go/bot"
	"her-go/config"
	"her-go/embed"
	"her-go/llm"
	"her-go/memory"
	"her-go/search"

	"github.com/spf13/cobra"
)

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
	// Load configuration from the file specified by --config.
	cfg, err := config.Load(cfgFile)
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Quick sanity checks — fail fast if critical config is missing.
	if cfg.Telegram.Token == "" {
		log.Fatalf("Telegram token is required. Set TELEGRAM_BOT_TOKEN env var or fill in config.yaml")
	}
	if cfg.LLM.APIKey == "" {
		log.Fatalf("LLM API key is required. Set OPENROUTER_API_KEY env var or fill in config.yaml")
	}

	// Initialize the SQLite database.
	store, err := memory.NewStore(cfg.Memory.DBPath)
	if err != nil {
		log.Fatalf("Failed to initialize database: %v", err)
	}
	defer store.Close()

	log.Printf("Database initialized at %s", cfg.Memory.DBPath)

	// Create the LLM client (conversational model — Deepseek).
	llmClient := llm.NewClient(
		cfg.LLM.BaseURL,
		cfg.LLM.APIKey,
		cfg.LLM.Model,
		cfg.LLM.Temperature,
		cfg.LLM.MaxTokens,
	)
	log.Printf("LLM client configured: %s (model: %s)", cfg.LLM.BaseURL, cfg.LLM.Model)

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
	log.Printf("Agent client configured: %s (model: %s)", cfg.LLM.BaseURL, agentModel)

	// Create the embedding client for semantic similarity.
	// Optional — if not configured, the agent skips duplicate checking.
	var embedClient *embed.Client
	if cfg.Embed.BaseURL != "" && cfg.Embed.Model != "" {
		embedClient = embed.NewClient(cfg.Embed.BaseURL, cfg.Embed.Model)
		log.Printf("Embed client configured: %s (model: %s, threshold: %.2f)",
			cfg.Embed.BaseURL, cfg.Embed.Model, cfg.Embed.SimilarityThreshold)
	} else {
		log.Println("Embed client not configured — semantic duplicate checking disabled")
	}

	// Create the Tavily client for web search and URL extraction.
	// Optional — if not configured, the agent's search tools will
	// return an error message instead of crashing.
	var tavilyClient *search.TavilyClient
	if cfg.Search.TavilyAPIKey != "" {
		tavilyClient = search.NewTavilyClient(cfg.Search.TavilyAPIKey, cfg.Search.TavilyBaseURL)
		log.Printf("Tavily client configured (web search enabled)")
	} else {
		log.Println("Tavily client not configured — web search disabled")
	}

	// Create and configure the Telegram bot.
	tgBot, err := bot.New(cfg, llmClient, agentClient, embedClient, tavilyClient, store)
	if err != nil {
		log.Fatalf("Failed to create Telegram bot: %v", err)
	}

	// Handle graceful shutdown.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	go func() {
		sig := <-sigChan
		log.Printf("Received %v, shutting down...", sig)
		tgBot.Stop()
	}()

	// Start the bot. This blocks until Stop() is called.
	tgBot.Start()
	log.Println("Bot stopped. Goodbye!")

	return nil
}
