package cmd

import (
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"os/signal"
	"syscall"
	"time"

	"her/agent"
	"her/bot"
	"her/config"
	"her/embed"
	"her/llm"
	"her/logger"
	"her/memory"
	"her/scheduler"
	"her/search"
	"her/voice"
	"her/weather"

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
	store, err := memory.NewStore(cfg.Memory.DBPath, cfg.Embed.Dimension)
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
	if cfg.LLM.Fallback != nil {
		llmClient.WithFallback(cfg.LLM.Fallback.Model, cfg.LLM.Fallback.Temperature, cfg.LLM.Fallback.MaxTokens)
		log.Info("LLM client configured", "url", cfg.LLM.BaseURL, "model", cfg.LLM.Model, "fallback", cfg.LLM.Fallback.Model)
	} else {
		log.Info("LLM client configured", "url", cfg.LLM.BaseURL, "model", cfg.LLM.Model)
	}

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
	if cfg.Agent.Fallback != nil {
		agentClient.WithFallback(cfg.Agent.Fallback.Model, cfg.Agent.Fallback.Temperature, cfg.Agent.Fallback.MaxTokens)
		log.Info("Agent client configured", "url", cfg.LLM.BaseURL, "model", agentModel, "fallback", cfg.Agent.Fallback.Model)
	} else {
		log.Info("Agent client configured", "url", cfg.LLM.BaseURL, "model", agentModel)
	}

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
		if cfg.Vision.Fallback != nil {
			visionClient.WithFallback(cfg.Vision.Fallback.Model, cfg.Vision.Fallback.Temperature, cfg.Vision.Fallback.MaxTokens)
			log.Info("Vision client configured", "model", cfg.Vision.Model, "fallback", cfg.Vision.Fallback.Model)
		} else {
			log.Info("Vision client configured", "model", cfg.Vision.Model)
		}
	} else {
		log.Info("Vision client not configured — image understanding disabled")
	}

	// Create the embedding client for semantic similarity.
	// Optional — if not configured, the agent skips duplicate checking.
	var embedClient *embed.Client
	if cfg.Embed.BaseURL != "" && cfg.Embed.Model != "" {
		embedClient = embed.NewClient(cfg.Embed.BaseURL, cfg.Embed.Model, cfg.Embed.Dimension)
		log.Info("Embed client configured", "url", cfg.Embed.BaseURL, "model", cfg.Embed.Model, "threshold", cfg.Embed.SimilarityThreshold)
	} else {
		log.Info("Embed client not configured — semantic duplicate checking disabled")
	}

	// Backfill embeddings for existing facts and populate vec_facts.
	// This runs at startup to ensure all facts are searchable via KNN.
	// On first run after upgrading to v0.4, this embeds all existing facts.
	// On subsequent runs, it only embeds new facts saved without vectors.
	if embedClient != nil && cfg.Embed.Dimension > 0 {
		go func() {
			unembedded, err := store.FactsWithoutEmbeddings()
			if err != nil {
				log.Error("backfill: failed to query unembedded facts", "err", err)
				return
			}
			if len(unembedded) == 0 {
				return
			}
			log.Infof("Backfilling embeddings for %d facts...", len(unembedded))
			for _, f := range unembedded {
				vec, err := embedClient.Embed(f.Fact)
				if err != nil {
					log.Error("backfill: embedding failed", "fact_id", f.ID, "err", err)
					continue
				}
				if err := store.UpdateFactEmbedding(f.ID, vec); err != nil {
					log.Error("backfill: update failed", "fact_id", f.ID, "err", err)
					continue
				}
			}
			log.Infof("Backfill complete: %d facts embedded", len(unembedded))
		}()
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

	// Create the weather client. Open-Meteo is free — no API key needed.
	// Returns nil if no location is configured (lat/lon both zero).
	weatherClient := weather.NewClient(
		cfg.Weather.Latitude, cfg.Weather.Longitude,
		cfg.Weather.TempUnit, cfg.Weather.WindSpeedUnit,
		cfg.Weather.CacheTTL,
	)
	if weatherClient != nil {
		log.Info("Weather client configured", "lat", cfg.Weather.Latitude, "lon", cfg.Weather.Longitude)
	}

	// Start the parakeet STT server if voice is enabled.
	// This launches parakeet-server as a child process — it loads the
	// MLX model into memory and serves an OpenAI-compatible transcription
	// endpoint. The process is killed on shutdown.
	//
	// Think of this like Python's subprocess.Popen() — we start it, keep
	// a reference, and kill it when we're done. The sidecar pattern keeps
	// ML inference (Python/MLX) separate from our Go process.
	var sttProcess *exec.Cmd
	voiceClient := voice.NewClient(&cfg.Voice)
	if voiceClient != nil {
		sttPath, err := exec.LookPath("parakeet-server")
		if err != nil {
			log.Warn("parakeet-server not found in PATH — voice memos will fail. Run: her setup")
		} else {
			// Parse host and port from the configured base_url so the
			// sidecar listens on the same address the client will call.
			// url.Parse splits "http://127.0.0.1:8765" into host="127.0.0.1"
			// and port="8765" — like Python's urllib.parse.urlparse().
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

			sttProcess = exec.Command(sttPath,
				"-m", cfg.Voice.STT.Model,
				"-h", sttHost,
				"-p", sttPort,
			)
			// Send sidecar output to our stderr so it shows up in logs.
			// In Go, os.Stderr is the raw file descriptor — same as
			// subprocess.Popen(stderr=subprocess.PIPE) but simpler.
			sttProcess.Stdout = os.Stderr
			sttProcess.Stderr = os.Stderr
			// Own process group so we can kill the entire tree on shutdown.
			sttProcess.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

			if err := sttProcess.Start(); err != nil {
				log.Error("failed to start parakeet-server", "err", err)
			} else {
				log.Info("parakeet-server started", "pid", sttProcess.Process.Pid, "model", cfg.Voice.STT.Model)

				// Give the server a moment to load the model before we
				// start accepting voice memos. The model loads into GPU
				// memory on first request anyway, but this avoids a
				// timeout on the very first voice memo.
				go func() {
					time.Sleep(3 * time.Second)
					if voiceClient.IsAvailable() {
						log.Info("parakeet-server is ready")
					} else {
						log.Warn("parakeet-server not responding yet — first voice memo may be slow")
					}
				}()
			}
		}
	}

	// Start the Kokoro TTS server if TTS is enabled.
	// We run our custom tts_server.py via `uv run` — uv reads the inline
	// script dependencies (PEP 723) and auto-installs piper-tts, fastapi,
	// etc. into an isolated env. The script loads the ONNX model from
	// scripts/ and exposes an OpenAI-compatible /v1/audio/speech endpoint.
	var ttsProcess *exec.Cmd
	ttsClient := voice.NewTTSClient(&cfg.Voice.TTS)
	if ttsClient != nil {
		uvPath, err := exec.LookPath("uv")
		if err != nil {
			log.Warn("uv not found in PATH — TTS will fail. Run: her setup")
		} else {
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

			// Resolve the script path relative to the working directory.
			ttsScript := filepath.Join("scripts", "tts_server.py")

			ttsProcess = exec.Command(uvPath, "run", ttsScript,
				"--host", ttsHost,
				"--port", ttsPort,
			)
			ttsProcess.Stdout = os.Stderr
			ttsProcess.Stderr = os.Stderr
			// Start the TTS server in its own process group so we can
			// kill the entire tree (uv + uvicorn) on shutdown, not just
			// the uv parent. Without this, uvicorn gets orphaned and
			// spills shutdown logs into the terminal after we've exited.
			ttsProcess.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

			if err := ttsProcess.Start(); err != nil {
				log.Error("failed to start TTS server", "err", err)
			} else {
				log.Info("piper TTS server started", "pid", ttsProcess.Process.Pid)

				go func() {
					time.Sleep(5 * time.Second)
					if ttsClient.IsAvailable() {
						log.Info("piper TTS server is ready")
					} else {
						log.Warn("piper TTS server not responding yet — first voice reply may be slow")
					}
				}()
			}
		}
	}

	// Create and configure the Telegram bot.
	tgBot, err := bot.New(cfg, cfgFile, llmClient, agentClient, visionClient, embedClient, tavilyClient, weatherClient, voiceClient, ttsClient, store)
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

		// sendFn delivers a plain text message to the owner's chat.
		sendFn := func(text string) error {
			return tgBot.SendToChat(ownerChat, text)
		}

		// agentFn runs a prompt through the full agent pipeline.
		// This is the callback for "run_prompt" scheduled tasks.
		//
		// The closure captures the same dependencies that bot.handleMessage
		// passes to agent.Run, but with a synthetic scheduled prompt
		// instead of a real user message. The StatusCallback sends a
		// NEW message (not edits a placeholder) since there's no
		// placeholder for scheduled runs.
		agentFn := func(prompt string) (string, error) {
			result, err := agent.Run(agent.RunParams{
				AgentLLM:            agentClient,
				ChatLLM:             llmClient,
				VisionLLM:           visionClient,
				Store:               store,
				EmbedClient:         embedClient,
				SimilarityThreshold: cfg.Embed.SimilarityThreshold,
				TavilyClient:        tavilyClient,
				WeatherClient:       weatherClient,
				Cfg:                 cfg,
				ScrubbedUserMessage: prompt, // the scheduled prompt IS the input
				ScrubVault:          nil,    // no PII scrubbing for system prompts
				ConversationID:      "scheduled",
				TriggerMsgID:        0,
				// For scheduled runs, the StatusCallback sends a new
				// Telegram message rather than editing a placeholder.
				// The agent's reply tool calls this to deliver the response.
				StatusCallback: func(text string) error {
					return tgBot.SendToChat(ownerChat, text)
				},
				TTSCallback:         nil, // no voice for scheduled messages (for now)
				ReflectionThreshold: cfg.Persona.ReflectionMemoryThreshold,
				RewriteEveryN:       cfg.Persona.RewriteEveryNReflections,
			})
			if err != nil {
				return "", err
			}
			return result.ReplyText, nil
		}

		// Build the list of default tasks from config flags.
		// Each enabled flag creates a system task on first startup.
		var defaults []scheduler.DefaultTask
		if cfg.Scheduler.MorningBriefing {
			defaults = append(defaults, scheduler.DefaultTask{
				Name:     "morning briefing",
				CronExpr: "0 8 * * *",
				TaskType: "run_prompt",
				Priority: "normal",
				Payload:  []byte(`{"prompt":"Generate a morning briefing for the user. Include anything relevant: weather if available, upcoming tasks, recent follow-ups worth mentioning. Keep it warm and concise — a few sentences, not a report."}`),
			})
		}
		if cfg.Scheduler.MoodCheckin {
			defaults = append(defaults, scheduler.DefaultTask{
				Name:     "mood check-in",
				CronExpr: "0 21 * * *",
				TaskType: "mood_checkin",
				Priority: "normal",
				Payload:  []byte(`{"style":"gentle","follow_up":true}`),
			})
		}
		if cfg.Scheduler.MedicationCheckin {
			defaults = append(defaults, scheduler.DefaultTask{
				Name:     "medication check-in",
				CronExpr: "0 21 * * *",
				TaskType: "medication_checkin",
				Priority: "critical",
				Payload:  []byte(`{"time_of_day":"evening"}`),
			})
		}
		if cfg.Scheduler.ProactiveFollowups {
			defaults = append(defaults, scheduler.DefaultTask{
				Name:     "proactive follow-ups",
				CronExpr: "0 9 * * *",
				TaskType: "run_prompt",
				Priority: "normal",
				Payload:  []byte(`{"prompt":"Scan facts from the last 48 hours with importance >= 7. If any warrant a follow-up (job interview, feeling rough, new medication, etc.), send a brief, warm check-in. If nothing stands out, do nothing — do NOT send a message just to say there's nothing to follow up on."}`),
			})
		}
		if cfg.Scheduler.AutoJournal {
			defaults = append(defaults, scheduler.DefaultTask{
				Name:     "auto-journal",
				CronExpr: "0 22 * * *",
				TaskType: "run_journal",
				Priority: "normal",
				Payload:  []byte(`{"style":"narrative"}`),
			})
		}

		// sendKeyboardFn sends messages with inline keyboards (mood check-ins,
		// medication check-ins, confirmations, etc.).
		sendKeyboardFn := func(msg scheduler.KeyboardMessage) error {
			return tgBot.SendKeyboardToChat(ownerChat, msg)
		}

		sched = scheduler.New(store, sendFn, sendKeyboardFn, agentFn, cfg.Scheduler.Timezone, scheduler.SchedulerOpts{
			QuietHoursStart:    cfg.Scheduler.QuietHoursStart,
			QuietHoursEnd:      cfg.Scheduler.QuietHoursEnd,
			MaxProactivePerDay: cfg.Scheduler.MaxProactivePerDay,
			Defaults:           defaults,
		})
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
		// Kill the STT sidecar if it's running. Process.Kill() sends
		// SIGKILL — immediate termination. We use Kill instead of a
		// graceful signal because the parakeet-server doesn't need
		// to flush anything, and we want a fast shutdown.
		if sttProcess != nil && sttProcess.Process != nil {
			log.Info("stopping parakeet-server", "pid", sttProcess.Process.Pid)
			_ = syscall.Kill(-sttProcess.Process.Pid, syscall.SIGKILL)
			_, _ = sttProcess.Process.Wait()
		}
		if ttsProcess != nil && ttsProcess.Process != nil {
			log.Info("stopping piper TTS server", "pid", ttsProcess.Process.Pid)
			// Kill the entire process group (negative PID) so uvicorn
			// dies with the uv parent — no orphaned shutdown logs.
			_ = syscall.Kill(-ttsProcess.Process.Pid, syscall.SIGKILL)
			_, _ = ttsProcess.Process.Wait()
		}
		tgBot.Stop()
	}()

	// Start the bot. This blocks until Stop() is called.
	tgBot.Start()
	log.Info("Bot stopped. Goodbye!")

	return nil
}
