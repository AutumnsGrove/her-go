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

	"her/bot"
	"her/config"
	"her/embed"
	"her/llm"
	"her/logger"
	"her/memory"
	"her/persona"
	"her/search"
	"her/tui"
	"her/voice"

	tea "charm.land/bubbletea/v2"
	"github.com/spf13/cobra"
	"golang.org/x/term"
	"gopkg.in/natefinch/lumberjack.v2"
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

	// Export config secrets as process-level env vars so skills can find
	// them via os.Getenv(). These die with the process — never touch the
	// parent shell, no cleanup needed.
	cfg.ExportEnv()

	// Enable debug mode if configured — logs full API request/response bodies.
	if cfg.Debug {
		llm.SetDebugMode(true)
		log.Info("debug mode enabled — full API context will be logged")
	}

	if cfg.Telegram.Token == "" {
		log.Fatal("Telegram token is required — set TELEGRAM_BOT_TOKEN env var or fill in config.yaml")
	}
	if cfg.LLM.APIKey == "" {
		log.Fatal("LLM API key is required — set OPENROUTER_API_KEY env var or fill in config.yaml")
	}

	// Kill any stale her process from a previous run before we start.
	// This prevents two instances racing for the same Telegram token.
	const herPIDFile = "her.pid"
	killStaleSelf(herPIDFile)
	if err := os.WriteFile(herPIDFile, []byte(strconv.Itoa(os.Getpid())), 0644); err != nil {
		log.Warn("failed to write PID file", "err", err)
	} else {
		defer os.Remove(herPIDFile)
	}

	store, err := memory.NewStore(cfg.Memory.DBPath, cfg.Embed.Dimension)
	if err != nil {
		log.Fatal("Failed to initialize database", "err", err)
	}
	defer store.Close()
	store.AutoLinkCount = cfg.Memory.AutoLinkCount
	store.AutoLinkThreshold = cfg.Memory.AutoLinkThreshold

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
		"agent_model", cfg.Agent.Model,
		"chat_model", cfg.LLM.Model,
		"classifier_model", cfg.Classifier.Model,
	)

	// Emit startup event now that the bus is live
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

	// --- Classifier client (optional) ---
	// Small, fast model that validates memory writes before they hit the DB.
	// Catches fictional content (game events, etc.) that the agent model
	// mistakes for real user facts.
	var classifierClient *llm.Client
	if cfg.Classifier.Model != "" {
		classifierMaxTokens := cfg.Classifier.MaxTokens
		if classifierMaxTokens == 0 {
			classifierMaxTokens = 64
		}
		classifierClient = llm.NewClient(cfg.LLM.BaseURL, cfg.LLM.APIKey, cfg.Classifier.Model, cfg.Classifier.Temperature, classifierMaxTokens)
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "classifier", Status: "ready", Detail: cfg.Classifier.Model})
	} else {
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "classifier", Status: "skipped"})
	}

	// --- Memory agent client (optional) ---
	// Post-turn background agent that reviews conversation turns and extracts
	// facts. Runs in a goroutine after the reply is sent — never blocks the user.
	var memoryAgentClient *llm.Client
	if cfg.MemoryAgent.Model != "" {
		maMaxTokens := cfg.MemoryAgent.MaxTokens
		if maMaxTokens == 0 {
			maMaxTokens = 4096
		}
		maTemp := cfg.MemoryAgent.Temperature
		if maTemp == 0 {
			maTemp = 0.3
		}
		memoryAgentClient = llm.NewClient(cfg.LLM.BaseURL, cfg.LLM.APIKey, cfg.MemoryAgent.Model, maTemp, maMaxTokens)
		if cfg.MemoryAgent.Fallback != nil {
			memoryAgentClient.WithFallback(cfg.MemoryAgent.Fallback.Model, cfg.MemoryAgent.Fallback.Temperature, cfg.MemoryAgent.Fallback.MaxTokens)
			bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "memory_agent", Status: "ready", Detail: cfg.MemoryAgent.Model + " (fallback: " + cfg.MemoryAgent.Fallback.Model + ")"})
		} else {
			bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "memory_agent", Status: "ready", Detail: cfg.MemoryAgent.Model})
		}
	} else {
		bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "memory_agent", Status: "skipped"})
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
				// Pass nil for embeddingText — this backfill only targets the tag
				// embedding (vec_facts). Text embeddings are populated on-demand
				// by checkDuplicate and FilterRedundantFacts.
				if err := store.UpdateFactEmbedding(f.ID, vec, nil); err != nil {
					log.Error("backfill: update failed", "fact_id", f.ID, "err", err)
					continue
				}
			}
			log.Infof("Backfill complete: %d facts embedded", len(unembedded))
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

	// --- Telegram bot ---

	tgBot, err := bot.New(cfg, cfgFile, llmClient, agentClient, memoryAgentClient, visionClient, classifierClient, embedClient, tavilyClient, voiceClient, ttsClient, store, bus)
	if err != nil {
		log.Error("Failed to create Telegram bot", "err", err)
		bus.Close()
		return
	}
	tgBot.SetOwnerChat(cfg.Telegram.OwnerChat)
	bus.Emit(tui.StartupEvent{Time: time.Now(), Phase: "telegram", Status: "ready"})

	// --- Dreamer ---
	// The dreamer goroutine runs nightly reflection and gated persona rewrites.
	// It needs a context so it shuts down cleanly when the bot exits.
	// We use the memory agent LLM (Kimi K2.5) for dreaming — same model, same
	// purpose (nuanced language processing), no need for a separate config entry.
	dreamerCtx, dreamerCancel := context.WithCancel(context.Background())
	if memoryAgentClient != nil {
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
		go persona.StartDreamer(dreamerCtx, persona.DreamerParams{
			LLM:       memoryAgentClient,
			Store:     store,
			Cfg:       cfg,
			EventBus:  bus,
			DreamHour: dreamHour,
			MinDays:   minDays,
			MinRefl:   minRefl,
		})
	} else {
		log.Warn("dreamer disabled — memory_agent.model not configured")
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
	dreamerCancel() // tell the dreamer goroutine to stop at its next wake-up

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
	if embedProcess != nil && embedProcess.Process != nil {
		log.Info("stopping embed sidecar", "pid", embedProcess.Process.Pid)
		_ = syscall.Kill(-embedProcess.Process.Pid, syscall.SIGKILL)
		_, _ = embedProcess.Process.Wait()
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
