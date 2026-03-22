// Package main is the entry point for her-go.
// It loads config, initializes the database, creates the LLM client
// and Telegram bot, and starts listening for messages.
package main

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"her-go/bot"
	"her-go/config"
	"her-go/llm"
	"her-go/memory"
)

func main() {
	// Load configuration from config.yaml.
	// os.Args would let us accept a custom path, but for simplicity
	// we hardcode it. Single binary, single config file.
	cfg, err := config.Load("config.yaml")
	if err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	// Quick sanity checks — fail fast if critical config is missing.
	// log.Fatalf prints the message and exits with code 1.
	// It's like sys.exit() with a print — used for unrecoverable errors
	// that should prevent the app from starting at all.
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
	// defer store.Close() ensures the database connection is closed when
	// main() returns. Even if we hit a fatal error below, the DB gets
	// closed cleanly. In Python you'd use "with" or atexit.
	defer store.Close()

	log.Printf("Database initialized at %s", cfg.Memory.DBPath)

	// Create the LLM client.
	llmClient := llm.NewClient(
		cfg.LLM.BaseURL,
		cfg.LLM.APIKey,
		cfg.LLM.Model,
		cfg.LLM.Temperature,
		cfg.LLM.MaxTokens,
	)
	log.Printf("LLM client configured: %s (model: %s)", cfg.LLM.BaseURL, cfg.LLM.Model)

	// Create and configure the Telegram bot.
	tgBot, err := bot.New(cfg, llmClient, store)
	if err != nil {
		log.Fatalf("Failed to create Telegram bot: %v", err)
	}

	// Handle graceful shutdown. When you press Ctrl+C (SIGINT) or the
	// system sends SIGTERM (e.g., during deployment), we want to stop
	// the bot cleanly instead of just killing the process.
	//
	// Channels are Go's way of communicating between goroutines — like
	// asyncio.Queue in Python. os/signal.Notify sends OS signals into
	// the channel, and we read from it in a goroutine.
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// This goroutine waits for a shutdown signal in the background.
	go func() {
		sig := <-sigChan // blocks until a signal arrives
		log.Printf("Received %v, shutting down...", sig)
		tgBot.Stop()
	}()

	// Start the bot. This blocks until Stop() is called (from the signal
	// handler above) or an unrecoverable error occurs.
	tgBot.Start()
	log.Println("Bot stopped. Goodbye!")
}
