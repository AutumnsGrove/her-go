package cmd

import (
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

	"her/agent"
	"her/bot"
	"her/config"
	"her/embed"
	"her/llm"
	"her/logger"
	"her/memory"
	"her/scheduler"
	"her/search"
	"her/tui"
	"her/voice"
	"her/weather"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"
	"golang.org/x/term"
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
	if cfg.Telegram.Token == "" {
		log.Fatal("Telegram token is required — set TELEGRAM_BOT_TOKEN env var or fill in config.yaml")
	}
	if cfg.LLM.APIKey == "" {
		log.Fatal("LLM API key is required — set OPENROUTER_API_KEY env var or fill in config.yaml")
	}

	store, err := memory.NewStore(cfg.Memory.DBPath, cfg.Embed.Dimension)
	if err != nil {
		log.Fatal("Failed to initialize database", "err", err)
	}
	defer store.Close()

	// --- Start the event bus and logger bridge ---
	// From this point on, all log.Info/Warn/Error calls flow through the bus.

	bus := tui.NewBus()

	// Open a log file for debugging when the TUI owns the terminal.
	logFile, err := os.OpenFile("her.log", os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err != nil {
		// Non-fatal — we can run without file logging
		log.Warn("could not open her.log for writing", "err", err)
		logger.Init(bus, nil)
	} else {
		defer logFile.Close()
		// Only pass bus, not logFile — the StartFileLogger subscriber
		// handles writing events to the file. Passing logFile to Init
		// would cause double-writes (logger bridge + file subscriber
		// both writing to the same file).
		logger.Init(bus, nil)
		tui.StartFileLogger(bus, logFile)
		// Sidecar output goes to the log file in TUI mode so it doesn't
		// corrupt the alt screen. In plain mode it stays on stderr.
		sidecarOut = logFile
	}

	// Emit a startup event now that the bus is live
	bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "db", Status: "ready", Detail: cfg.Memory.DBPath})

	// --- Decide: TUI or plain fallback ---
	// If stdout is not a terminal (piped, redirected, CI), skip the TUI
	// and let the file logger handle everything.
	useTUI := term.IsTerminal(int(os.Stdout.Fd()))

	if !useTUI {
		// Plain mode — run init and bot directly on this goroutine
		return runBotPlain(cfg, store, bus)
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
		runBotBackground(cfg, store, bus, program, quitCh)
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
func runBotBackground(cfg *config.Config, store *memory.Store, bus *tui.Bus, program *tea.Program, quitCh chan struct{}) {
	// --- Create LLM clients ---

	llmClient := llm.NewClient(cfg.LLM.BaseURL, cfg.LLM.APIKey, cfg.LLM.Model, cfg.LLM.Temperature, cfg.LLM.MaxTokens)
	if cfg.LLM.Fallback != nil {
		llmClient.WithFallback(cfg.LLM.Fallback.Model, cfg.LLM.Fallback.Temperature, cfg.LLM.Fallback.MaxTokens)
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "llm", Status: "ready", Detail: cfg.LLM.Model + " (fallback: " + cfg.LLM.Fallback.Model + ")"})
	} else {
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "llm", Status: "ready", Detail: cfg.LLM.Model})
	}

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
	agentClient := llm.NewClient(cfg.LLM.BaseURL, cfg.LLM.APIKey, agentModel, agentTemp, agentMaxTokens)
	if cfg.Agent.Fallback != nil {
		agentClient.WithFallback(cfg.Agent.Fallback.Model, cfg.Agent.Fallback.Temperature, cfg.Agent.Fallback.MaxTokens)
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "agent", Status: "ready", Detail: agentModel + " (fallback: " + cfg.Agent.Fallback.Model + ")"})
	} else {
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "agent", Status: "ready", Detail: agentModel})
	}

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
		visionClient = llm.NewClient(cfg.LLM.BaseURL, cfg.LLM.APIKey, cfg.Vision.Model, visionTemp, visionMaxTokens)
		if cfg.Vision.Fallback != nil {
			visionClient.WithFallback(cfg.Vision.Fallback.Model, cfg.Vision.Fallback.Temperature, cfg.Vision.Fallback.MaxTokens)
		}
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "vision", Status: "ready", Detail: cfg.Vision.Model})
	} else {
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "vision", Status: "skipped"})
	}

	// --- Embedding client ---

	var embedClient *embed.Client
	if cfg.Embed.BaseURL != "" && cfg.Embed.Model != "" {
		embedClient = embed.NewClient(cfg.Embed.BaseURL, cfg.Embed.Model, cfg.Embed.Dimension)
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "embed", Status: "ready", Detail: cfg.Embed.Model})
	} else {
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "embed", Status: "skipped"})
	}

	// Backfill embeddings in background
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
				// Embed by tags when available (topic-based retrieval),
				// fall back to fact text for un-tagged facts.
				embedText := f.Tags
				if embedText == "" {
					embedText = f.Fact
				}
				vec, err := embedClient.Embed(embedText)
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

	// --- Search + weather ---

	var tavilyClient *search.TavilyClient
	if cfg.Search.TavilyAPIKey != "" {
		tavilyClient = search.NewTavilyClient(cfg.Search.TavilyAPIKey, cfg.Search.TavilyBaseURL)
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "search", Status: "ready"})
	} else {
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "search", Status: "skipped"})
	}

	weatherClient := weather.NewClient(
		cfg.Weather.Latitude, cfg.Weather.Longitude,
		cfg.Weather.TempUnit, cfg.Weather.WindSpeedUnit,
		cfg.Weather.CacheTTL,
	)
	if weatherClient != nil {
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "weather", Status: "ready"})
	}

	// --- Sidecars (STT/TTS) with pipe capture ---

	var sttProcess *exec.Cmd
	voiceClient := voice.NewClient(&cfg.Voice)
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

	// --- Telegram bot ---

	tgBot, err := bot.New(cfg, cfgFile, llmClient, agentClient, visionClient, embedClient, tavilyClient, weatherClient, voiceClient, ttsClient, store, bus)
	if err != nil {
		log.Error("Failed to create Telegram bot", "err", err)
		bus.Close()
		return
	}
	bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "telegram", Status: "ready"})

	// --- Scheduler ---

	var sched *scheduler.Scheduler
	if cfg.Telegram.OwnerChat != 0 {
		ownerChat := cfg.Telegram.OwnerChat
		sendFn := func(text string) error { return tgBot.SendToChat(ownerChat, text) }
		agentFn := func(prompt string) (string, error) {
			result, err := agent.Run(agent.RunParams{
				AgentLLM: agentClient, ChatLLM: llmClient, VisionLLM: visionClient,
				Store: store, EmbedClient: embedClient,
				SimilarityThreshold: cfg.Embed.SimilarityThreshold,
				TavilyClient:        tavilyClient, WeatherClient: weatherClient, Cfg: cfg,
				ScrubbedUserMessage: prompt, ScrubVault: nil,
				ConversationID: "scheduled", TriggerMsgID: 0,
				StatusCallback:      func(text string) error { return tgBot.SendToChat(ownerChat, text) },
				ReflectionThreshold: cfg.Persona.ReflectionMemoryThreshold,
				RewriteEveryN:       cfg.Persona.RewriteEveryNReflections,
				EventBus:            bus,
			})
			if err != nil {
				return "", err
			}
			return result.ReplyText, nil
		}

		var defaults []scheduler.DefaultTask
		if cfg.Scheduler.MorningBriefing {
			defaults = append(defaults, scheduler.DefaultTask{
				Name: "morning briefing", CronExpr: "0 8 * * *", TaskType: "run_prompt", Priority: "normal",
				Payload: []byte(`{"prompt":"Generate a morning briefing for the user. Include anything relevant: weather if available, upcoming tasks, recent follow-ups worth mentioning. Keep it warm and concise — a few sentences, not a report."}`),
			})
		}
		if cfg.Scheduler.MoodCheckin {
			defaults = append(defaults, scheduler.DefaultTask{
				Name: "mood check-in", CronExpr: "0 21 * * *", TaskType: "mood_checkin", Priority: "normal",
				Payload: []byte(`{"style":"gentle","follow_up":true}`),
			})
		}
		if cfg.Scheduler.MedicationCheckin {
			defaults = append(defaults, scheduler.DefaultTask{
				Name: "medication check-in", CronExpr: "0 21 * * *", TaskType: "medication_checkin", Priority: "critical",
				Payload: []byte(`{"time_of_day":"evening"}`),
			})
		}
		if cfg.Scheduler.ProactiveFollowups {
			defaults = append(defaults, scheduler.DefaultTask{
				Name: "proactive follow-ups", CronExpr: "0 9 * * *", TaskType: "run_prompt", Priority: "normal",
				Payload: []byte(`{"prompt":"Scan facts from the last 48 hours with importance >= 7. If any warrant a follow-up (job interview, feeling rough, new medication, etc.), send a brief, warm check-in. If nothing stands out, do nothing — do NOT send a message just to say there's nothing to follow up on."}`),
			})
		}
		if cfg.Scheduler.AutoJournal {
			defaults = append(defaults, scheduler.DefaultTask{
				Name: "auto-journal", CronExpr: "0 22 * * *", TaskType: "run_journal", Priority: "normal",
				Payload: []byte(`{"style":"narrative"}`),
			})
		}

		sendKeyboardFn := func(msg scheduler.KeyboardMessage) error {
			return tgBot.SendKeyboardToChat(ownerChat, msg)
		}

		sched = scheduler.New(store, sendFn, sendKeyboardFn, agentFn, tgBot.IsAgentBusy, cfg.Scheduler.Timezone, scheduler.SchedulerOpts{
			QuietHoursStart: cfg.Scheduler.QuietHoursStart, QuietHoursEnd: cfg.Scheduler.QuietHoursEnd,
			MaxProactivePerDay: cfg.Scheduler.MaxProactivePerDay, Defaults: defaults,
		})
		sched.Start()
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "scheduler", Status: "ready"})
	} else {
		log.Warn("scheduler disabled — set telegram.owner_chat in config.yaml (use /status to find your chat ID)")
	}

	// --- Signal handling + bot start ---
	// Listen for SIGINT/SIGTERM. When received, shut everything down
	// and close the bus (which makes the TUI exit).

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start the bot in yet another goroutine — tgBot.Start() blocks.
	botDone := make(chan struct{})
	go func() {
		tgBot.Start()
		close(botDone)
	}()

	// Wait for either: signal received OR TUI quit (user pressed q)
	select {
	case sig := <-sigChan:
		log.Info("Signal received, shutting down", "signal", sig)
	case <-quitCh:
		log.Info("TUI quit requested, shutting down")
	}

	// --- Cleanup ---
	if sched != nil {
		sched.Stop()
	}
	if sttProcess != nil && sttProcess.Process != nil {
		log.Info("stopping parakeet-server", "pid", sttProcess.Process.Pid)
		_ = syscall.Kill(-sttProcess.Process.Pid, syscall.SIGKILL)
		_, _ = sttProcess.Process.Wait()
	}
	if ttsProcess != nil && ttsProcess.Process != nil {
		log.Info("stopping piper TTS server", "pid", ttsProcess.Process.Pid)
		_ = syscall.Kill(-ttsProcess.Process.Pid, syscall.SIGKILL)
		_, _ = ttsProcess.Process.Wait()
	}
	tgBot.Stop()
	<-botDone // wait for tgBot.Start() to return

	bus.Close() // closes event channels → TUI exits
}

// runBotPlain runs the bot without a TUI (piped output, CI, etc.).
// Same logic as the original runBot but with events going to file logger only.
func runBotPlain(cfg *config.Config, store *memory.Store, bus *tui.Bus) error {
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

// startSTTSidecar launches the parakeet-server process.
// Output goes to sidecarOut (log file in TUI mode, stderr in plain mode).
func startSTTSidecar(cfg *config.Config, bus *tui.Bus, voiceClient *voice.Client) *exec.Cmd {
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
	ttsProcess := exec.Command(uvPath, "run", ttsScript, "--host", ttsHost, "--port", ttsPort)
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
