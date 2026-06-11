package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"her/calendar"
	"her/config"
	"her/embed"
	"her/gateway"
	"her/llm"
	"her/memory"
	"her/search"
	"her/tui"
	"her/voice"
	"her/workeragent"

	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// simGWCmd is the "her sim" subcommand — runs a suite through the full
// gateway pipeline (driver agent → pipeline → sim adapter). This is the
// recommended path over sim-legacy because it exercises the same code
// that production uses, just with a clean-room temp database and no
// Telegram or voice sidecars.
var simGWCmd = &cobra.Command{
	Use:   "sim",
	Short: "Run a simulation through the gateway pipeline",
	Long: `Runs a suite YAML through the full gateway pipeline using a sim adapter.
Exercises the same agent + pipeline code as production, but in a clean-room
temp database with no Telegram, voice, or D1 sync.

Example:
  her sim --suite sims/getting-to-know-you.yaml`,
	RunE: runSimGW,
}

func init() {
	f := simGWCmd.Flags()
	f.StringVarP(&suiteFlag, "suite", "s", "", "path to suite YAML file (required)")
	f.IntVarP(&limitFlag, "limit", "n", 0, "max messages to send (0 = all)")
	f.IntVarP(&delayFlag, "delay", "d", 1, "seconds to wait between turns")
	f.StringVar(&driverModelFlag, "driver-model", "", "override driver agent model")
	f.StringVar(&chatModelFlag, "chat-model", "", "override chat (reply) model")
	f.StringVar(&memoryModelFlag, "memory-model", "", "override memory agent model")
	f.StringVar(&moodModelFlag, "mood-model", "", "override mood agent model")
	f.StringVar(&introspectionModelFlag, "introspection-model", "", "override introspection agent model")
	f.StringVar(&classifierModelFlag, "classifier-model", "", "override classifier model")
	f.StringVar(&embedModelFlag, "embed-model", "", "override embedding model")
	f.StringVar(&embedBaseURLFlag, "embed-base-url", "", "override embedding API base URL")
	f.StringVar(&embedAPIKeyFlag, "embed-api-key", "", "API key for remote embedding APIs")
	f.IntVar(&embedDimensionFlag, "embed-dimension", 0, "override embedding dimension")
	f.StringVar(&chatProviderFlag, "chat-provider", "", "pin chat model to OpenRouter provider(s), comma-separated")
	f.StringVar(&fallbackModelFlag, "fallback-model", "", "override fallback model for all roles")
	f.StringVar(&fallbackVisionModelFlag, "fallback-vision-model", "", "override fallback model for vision")
	f.BoolVar(&disableReasoningFlag, "disable-reasoning", false, "disable reasoning mode for hybrid models")
	f.BoolVar(&directReplyFlag, "direct-reply", false, "enable reply_direct tool — driver writes reply text directly (experimental)")
	f.BoolVar(&fastPathFlag, "fast-path", false, "enable fast-path classifier — route simple messages directly to chat model, skip driver")
	f.BoolVar(&noFastPathFlag, "no-fast-path", false, "disable fast-path classifier (overrides config.yaml)")
	simGWCmd.MarkFlagRequired("suite")
	rootCmd.AddCommand(simGWCmd)
}

// runSimGW is the entry point for "her sim". The flow is:
//  1. Load config + suite YAML
//  2. Create a temp SQLite DB (clean-room — no bleed from her.db)
//  3. Build shared LLM/embed/search clients from config
//  4. Create a gateway with a single sim adapter
//  5. Run the gateway; wait for the sim adapter to signal Done
//  6. Cancel the gateway context and print results
func runSimGW(cmd *cobra.Command, args []string) error {
	startTime := time.Now()

	// --- Load config ---
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	cfg.ExportEnv()

	if cfg.OpenRouter.APIKey == "" {
		return fmt.Errorf("LLM API key is required — set OPENROUTER_API_KEY or fill in config.yaml")
	}

	// --- Apply CLI flag overrides ---
	applySimModelOverrides(cfg)

	// --- Load suite YAML ---
	suiteData, err := os.ReadFile(suiteFlag)
	if err != nil {
		return fmt.Errorf("reading suite file %q: %w", suiteFlag, err)
	}

	// We only need the name, description, and messages for this command.
	// The full suite struct (with seed_memories, seed_cards, etc.) is
	// defined in cmd/sim.go and shared by both commands — same package.
	var s suite
	if err := yaml.Unmarshal(suiteData, &s); err != nil {
		return fmt.Errorf("parsing suite YAML: %w", err)
	}

	if len(s.Messages) == 0 {
		return fmt.Errorf("suite %q has no messages", s.Name)
	}

	log.Infof("sim: suite %q — %d message(s)", s.Name, len(s.Messages))

	// --- Temp database ---
	// os.MkdirTemp creates a directory under the OS temp root (e.g. /tmp)
	// with a unique suffix. We store sim.db inside it so cleanup is one
	// os.RemoveAll call. This is the clean-room guarantee: each sim run
	// starts with a brand-new empty database, no residue from her.db.
	tmpDir, err := os.MkdirTemp("", "her-sim-*")
	if err != nil {
		return fmt.Errorf("creating temp dir: %w", err)
	}
	// Cleanup deferred — if the run completes, we move the DB to
	// sims/results/ instead. The defer only fires as a safety net
	// for early-return error paths.
	defer os.RemoveAll(tmpDir)

	tmpDBPath := filepath.Join(tmpDir, "sim.db")

	cfg.Gateway.Adapters = []config.AdapterConfig{{
		Name: "sim",
		Type: "sim",
		DB:   tmpDBPath,
	}}

	// --- Pre-seed the database ---
	// Open the store, seed memories/cards/persona, then register it with
	// the gateway so it reuses the pre-seeded store instead of opening a
	// new one.
	store, err := memory.NewStore(tmpDBPath, cfg.Embed.Dimension)
	if err != nil {
		return fmt.Errorf("opening temp store: %w", err)
	}

	embedClient := embed.NewClient(cfg.Embed.BaseURL, cfg.Embed.Model, cfg.Embed.APIKey, cfg.Embed.Dimension)

	personaCleanup, err := seedSimDB(store, embedClient, cfg, s)
	if err != nil {
		return fmt.Errorf("seeding sim database: %w", err)
	}
	if personaCleanup != nil {
		defer personaCleanup()
	}

	// --- LLM clients ---
	// Same pattern as run.go: each role gets its own client so they can have
	// independent models, temperatures, timeouts, and fallbacks. In Python
	// you'd probably use a dict; in Go we use explicit named fields in Deps.

	chatLLM := llm.NewClient(cfg.OpenRouter.BaseURL, cfg.OpenRouter.APIKey,
		cfg.Chat.Model, cfg.Chat.Temperature, cfg.Chat.MaxTokens)
	if cfg.Chat.Timeout > 0 {
		chatLLM.WithTimeout(time.Duration(cfg.Chat.Timeout) * time.Second)
	}
	if cfg.Chat.Fallback != nil {
		chatLLM.WithFallback(cfg.Chat.Fallback.Model, cfg.Chat.Fallback.Temperature, cfg.Chat.Fallback.MaxTokens)
	}
	if cfg.Chat.Provider != nil {
		chatLLM.WithProvider(&llm.ProviderRouting{Order: cfg.Chat.Provider.Order, Only: cfg.Chat.Provider.Only, Sort: cfg.Chat.Provider.Sort})
	}
	if chatProviderFlag != "" {
		providers := strings.Split(chatProviderFlag, ",")
		chatLLM.WithProvider(&llm.ProviderRouting{Order: providers})
		log.Infof("sim: chat provider → %v", providers)
	}
	if disableReasoningFlag {
		disabled := false
		chatLLM.WithReasoning(&llm.ReasoningControl{Enabled: &disabled})
	}

	driverLLM := llm.NewClient(cfg.OpenRouter.BaseURL, cfg.OpenRouter.APIKey,
		cfg.Driver.Model, cfg.Driver.Temperature, cfg.Driver.MaxTokens)
	if cfg.Driver.Timeout > 0 {
		driverLLM.WithTimeout(time.Duration(cfg.Driver.Timeout) * time.Second)
	}
	if cfg.Driver.Fallback != nil {
		driverLLM.WithFallback(cfg.Driver.Fallback.Model, cfg.Driver.Fallback.Temperature, cfg.Driver.Fallback.MaxTokens)
	}
	if disableReasoningFlag {
		disabled := false
		driverLLM.WithReasoning(&llm.ReasoningControl{Enabled: &disabled})
	}

	var memoryAgentLLM *llm.Client
	if cfg.MemoryAgent.Model != "" {
		memoryAgentLLM = llm.NewClient(cfg.OpenRouter.BaseURL, cfg.OpenRouter.APIKey,
			cfg.MemoryAgent.Model, cfg.MemoryAgent.Temperature, cfg.MemoryAgent.MaxTokens)
		if cfg.MemoryAgent.Timeout > 0 {
			memoryAgentLLM.WithTimeout(time.Duration(cfg.MemoryAgent.Timeout) * time.Second)
		}
		if cfg.MemoryAgent.Fallback != nil {
			memoryAgentLLM.WithFallback(cfg.MemoryAgent.Fallback.Model, cfg.MemoryAgent.Fallback.Temperature, cfg.MemoryAgent.Fallback.MaxTokens)
		}
	}

	var moodAgentLLM *llm.Client
	if cfg.MoodAgent.Model != "" {
		mTemp := cfg.MoodAgent.Temperature
		if mTemp == 0 {
			mTemp = 0.2
		}
		mTokens := cfg.MoodAgent.MaxTokens
		if mTokens == 0 {
			mTokens = 512
		}
		moodAgentLLM = llm.NewClient(cfg.OpenRouter.BaseURL, cfg.OpenRouter.APIKey,
			cfg.MoodAgent.Model, mTemp, mTokens)
		if cfg.MoodAgent.Timeout > 0 {
			moodAgentLLM.WithTimeout(time.Duration(cfg.MoodAgent.Timeout) * time.Second)
		}
	}

	var visionLLM *llm.Client
	if cfg.Vision.Model != "" {
		visionLLM = llm.NewClient(cfg.OpenRouter.BaseURL, cfg.OpenRouter.APIKey,
			cfg.Vision.Model, cfg.Vision.Temperature, cfg.Vision.MaxTokens)
		if cfg.Vision.Fallback != nil {
			visionLLM.WithFallback(cfg.Vision.Fallback.Model, cfg.Vision.Fallback.Temperature, cfg.Vision.Fallback.MaxTokens)
		}
	}

	var classifierLLM *llm.Client
	if cfg.Classifier.Model != "" {
		classifierLLM = llm.NewClient(cfg.OpenRouter.BaseURL, cfg.OpenRouter.APIKey,
			cfg.Classifier.Model, cfg.Classifier.Temperature, cfg.Classifier.MaxTokens)
	}

	var dreamAgentLLM *llm.Client
	if cfg.DreamAgent.Model != "" {
		dreamAgentLLM = llm.NewClient(cfg.OpenRouter.BaseURL, cfg.OpenRouter.APIKey,
			cfg.DreamAgent.Model, cfg.DreamAgent.Temperature, cfg.DreamAgent.MaxTokens)
		timeout := cfg.DreamAgent.Timeout
		if timeout == 0 {
			timeout = 120
		}
		dreamAgentLLM.WithTimeout(time.Duration(timeout) * time.Second)
	}

	var introspectionLLM *llm.Client
	if cfg.IntrospectionAgent.Model != "" {
		introspectionLLM = llm.NewClient(cfg.OpenRouter.BaseURL, cfg.OpenRouter.APIKey,
			cfg.IntrospectionAgent.Model, cfg.IntrospectionAgent.Temperature, cfg.IntrospectionAgent.MaxTokens)
		timeout := cfg.IntrospectionAgent.Timeout
		if timeout == 0 {
			timeout = 60
		}
		introspectionLLM.WithTimeout(time.Duration(timeout) * time.Second)
	}

	// --- Search client ---
	tavilyClient := search.NewTavilyClient(cfg.Search.TavilyAPIKey, cfg.Search.TavilyBaseURL)

	// --- Calendar FakeBridge ---
	// Create an in-memory calendar bridge for sims. This bypasses the
	// Swift EventKit binary and lets calendar tools work without macOS
	// dependencies. Events were already seeded into SQLite by seedSimDB;
	// now we also seed them into the FakeBridge so calendar_list works.
	var fakeBridge *calendar.FakeBridge
	if len(s.SeedCalendarEvents) > 0 && len(cfg.Calendar.Calendars) > 0 {
		fakeBridge = calendar.NewFakeBridge(cfg.Calendar.Calendars)
		var fakeEvents []*calendar.FakeEvent
		for _, seed := range s.SeedCalendarEvents {
			start, err := time.Parse(time.RFC3339, seed.Start)
			if err != nil {
				continue
			}
			end, err := time.Parse(time.RFC3339, seed.End)
			if err != nil {
				continue
			}
			cal := seed.Calendar
			if cal == "" {
				cal = cfg.Calendar.DefaultCalendar
			}
			fakeEvents = append(fakeEvents, &calendar.FakeEvent{
				ID: seed.ID, Title: seed.Title,
				Start: start, End: end,
				Location: seed.Location, Notes: seed.Notes,
				Calendar: cal,
			})
		}
		if len(fakeEvents) > 0 {
			fakeBridge.Seed(fakeEvents)
		}
		log.Infof("sim: FakeBridge created with %d events", len(fakeEvents))
	}

	// --- Worker agent ---
	simRootDir, _ := os.Getwd()
	_ = workeragent.Init(simRootDir)

	simWorkerLLMs := map[string]*llm.Client{}
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
		simWorkerLLMs[tier] = c
	}

	simReportsDir := filepath.Join(simRootDir, "reports")
	if cfg.WorkerAgent.ReportsDir != "" {
		simReportsDir = filepath.Join(simRootDir, cfg.WorkerAgent.ReportsDir)
	}

	var simWorkerCB func(taskType, note string)
	workerResultCh := make(chan gateway.WorkerResult, 1)
	if len(simWorkerLLMs) > 0 {
		simWorkerCB = func(taskType, note string) {
			tt := workeragent.Lookup(taskType)
			if tt == nil {
				log.Error("sim worker: unknown task type", "type", taskType)
				return
			}
			llmClient := simWorkerLLMs[tt.ModelTier]
			if llmClient == nil {
				log.Error("sim worker: no LLM for tier", "tier", tt.ModelTier)
				return
			}
			log.Info("sim worker: running", "task", taskType, "tier", tt.ModelTier)
			result := workeragent.RunWorker(workeragent.WorkerInput{
				TaskType:    taskType,
				Instruction: note,
			}, workeragent.WorkerParams{
				LLM:          llmClient,
				TavilyClient: tavilyClient,
				Store:        store,
				Cfg:          cfg,
				ReportsDir:   simReportsDir,
			})
			log.Info("sim worker: done", "report", result.ReportPath, "success", result.Success)

			// Emit result so the sim adapter can inject a follow-up turn.
			select {
			case workerResultCh <- gateway.WorkerResult{
				TaskName: taskType,
				Summary:  result.Summary,
			}:
			default:
			}
		}
	}

	// --- TTS client (optional, for narrate_report support in sims) ---
	var simTTSClient *voice.TTSClient
	if cfg.Voice.TTS.Enabled {
		simTTSClient = voice.NewTTSClient(&cfg.Voice.TTS)
	}

	// --- Assemble deps ---
	deps := gateway.Deps{
		ChatLLM:          chatLLM,
		DriverLLM:        driverLLM,
		MemoryAgentLLM:   memoryAgentLLM,
		MoodAgentLLM:     moodAgentLLM,
		VisionLLM:        visionLLM,
		ClassifierLLM:    classifierLLM,
		DreamAgentLLM:    dreamAgentLLM,
		IntrospectionLLM: introspectionLLM,
		EmbedClient:      embedClient,
		TavilyClient:     tavilyClient,
		CalendarBridge:   fakeBridge,
		ConfigPath:       cfgFile,
		WorkerCallback:   simWorkerCB,
		WorkerResultCh:   workerResultCh,
		TTSClient:        simTTSClient,
		// VoiceClient intentionally nil — no STT in sim mode.
	}

	// --- Convert suite messages to gateway.SimMessage ---
	msgs := s.Messages
	if limitFlag > 0 && limitFlag < len(msgs) {
		log.Infof("sim: limiting to %d/%d messages (--limit)", limitFlag, len(msgs))
		msgs = msgs[:limitFlag]
	}
	gwMessages := make([]gateway.SimMessage, len(msgs))
	for i, m := range msgs {
		gwMessages[i] = gateway.SimMessage{
			Text:  m.Text,
			Image: m.Image,
		}
	}

	// --- Build and run gateway ---
	// tui.NewBus() gives us a live event bus even without the TUI running.
	// The gateway and pipeline publish events to it; without subscribers
	// they're discarded. This means trace output won't show in sim mode,
	// which is intentional — keep output clean and results-focused.
	bus := tui.NewBus()
	gw := gateway.New(cfg, deps, bus)
	gw.RegisterStore(tmpDBPath, store)
	gw.SimMessages = gwMessages
	gw.SimTriggers = gateway.SimTriggers{
		CompactAfter: s.CompactAfter,
		DreamAfter:   s.DreamAfter,
		RunDream:     s.RunDream,
		RunRollup:    s.RunRollup,
	}
	gw.SimOptions = gateway.SimOptions{
		DelaySeconds: delayFlag,
	}

	// context.WithCancel lets us stop the gateway once the sim adapter
	// signals Done. In Go, ctx.Done() is a channel — when we call cancel(),
	// the channel closes and anything blocking on <-ctx.Done() unblocks.
	// This is how cooperative cancellation works across goroutines.
	ctx, cancel := context.WithCancel(context.Background())

	// Wait for the sim adapter to finish, then cancel the gateway context.
	// This goroutine bridges the sim adapter's Done channel (closed when all
	// messages are processed) to the gateway's cancellation signal.
	gwErrCh := make(chan error, 1)
	go func() {
		gwErrCh <- gw.Run(ctx)
	}()

	// Wait until adapters are ready, then find the sim adapter.
	<-gw.Ready
	sa := gw.SimAdapter()
	if sa == nil {
		cancel()
		return fmt.Errorf("sim adapter not found after gateway start")
	}

	// Block until all messages are processed or the gateway exits early.
	// We use a separate gatewayErr variable so both branches can drain
	// gwErrCh exactly once — the select drains it in the error branch,
	// and the explicit <-gwErrCh drains it in the happy path.
	var gatewayErr error
	select {
	case <-sa.Done:
		cancel()               // sim finished normally — signal gateway to stop
		gatewayErr = <-gwErrCh // drain; gw.Run returns nil after ctx cancel
	case gatewayErr = <-gwErrCh:
		cancel() // gateway exited before sim finished — cancel context anyway
		if gatewayErr != nil {
			return fmt.Errorf("gateway exited unexpectedly: %w", gatewayErr)
		}
	}
	_ = gatewayErr

	// --- Print results + generate report ---
	elapsed := time.Since(startTime)
	results := sa.Results()
	printSimResults(s.Name, results)

	reportPath, err := generateSimReport(cfg, s, results, store, elapsed)
	if err != nil {
		log.Errorf("sim: report generation failed: %v", err)
	} else {
		fmt.Printf("Report: %s\n", reportPath)
	}

	// Preserve the sim database alongside the report for post-mortem
	// analysis. Close the store first so SQLite flushes WAL, then copy
	// the DB file to sims/results/. The defer os.RemoveAll still cleans
	// up the temp dir (including WAL/SHM files).
	store.Close()
	if reportPath != "" {
		dbDest := strings.TrimSuffix(reportPath, ".md") + ".db"
		src, err := os.ReadFile(tmpDBPath)
		if err != nil {
			log.Warnf("sim: failed to read temp DB for copy: %v", err)
		} else if err := os.WriteFile(dbDest, src, 0o644); err != nil {
			log.Warnf("sim: failed to copy DB to %s: %v", dbDest, err)
		} else {
			fmt.Printf("Database: %s\n", dbDest)
		}
	}

	return nil
}

// printSimResults renders a detailed table of sim turn results to stdout.
// Shows per-turn costs, tool counts, mood verdicts, and memory activity.
func printSimResults(suiteName string, results []gateway.SimResult) {
	passed := 0
	failed := 0
	totalCost := 0.0
	totalTools := 0
	for _, r := range results {
		if r.Error == nil {
			passed++
		} else {
			failed++
		}
		totalCost += r.Cost
		totalTools += r.ToolCalls
	}

	fmt.Printf("\n=== Sim Results: %s ===\n", suiteName)
	fmt.Printf("Turns: %d  Passed: %d  Failed: %d  Cost: $%.4f  Tools: %d\n\n",
		len(results), passed, failed, totalCost, totalTools)

	for i, r := range results {
		if r.Error != nil {
			fmt.Printf("[%d/%d] ERR   %s\n", i+1, len(results), truncSimText(r.Input, 60))
			fmt.Printf("         error: %v\n", r.Error)
			continue
		}

		fmt.Printf("[%d/%d] OK    %s\n", i+1, len(results), truncSimText(r.Input, 60))
		fmt.Printf("         reply (%s, $%.4f, %d tools): %s\n",
			r.Duration.Round(time.Millisecond), r.Cost, r.ToolCalls,
			truncSimText(r.Reply, 80))

		// Mood verdict
		if r.MoodVerdict != "" {
			labels := strings.Join(r.MoodLabels, ", ")
			if labels != "" {
				fmt.Printf("         mood: %s v=%d [%s]\n", r.MoodVerdict, r.MoodValence, labels)
			} else {
				fmt.Printf("         mood: %s\n", r.MoodVerdict)
			}
		}

		// Memories saved
		for _, m := range r.MemoriesSaved {
			fmt.Printf("         💾 %s\n", truncSimText(m, 70))
		}

		// Follow-up reply
		if r.FollowUpReply != "" {
			fmt.Printf("         📨 follow-up: %s\n", truncSimText(r.FollowUpReply, 70))
		}

		// Introspection memories
		for _, m := range r.IntrospectionSaved {
			fmt.Printf("         🪡 %s\n", truncSimText(m, 70))
		}
	}

	fmt.Println()
}

func truncSimText(s string, max int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}

// applySimModelOverrides applies --driver-model, --chat-model, etc.
// flags to the config before LLM clients are built.
func applySimModelOverrides(cfg *config.Config) {
	if driverModelFlag != "" {
		cfg.Driver.Model = driverModelFlag
		log.Infof("sim: driver model → %s", driverModelFlag)
	}
	if chatModelFlag != "" {
		cfg.Chat.Model = chatModelFlag
		log.Infof("sim: chat model → %s", chatModelFlag)
	}
	if memoryModelFlag != "" {
		cfg.MemoryAgent.Model = memoryModelFlag
		log.Infof("sim: memory model → %s", memoryModelFlag)
	}
	if moodModelFlag != "" {
		cfg.MoodAgent.Model = moodModelFlag
		log.Infof("sim: mood model → %s", moodModelFlag)
	}
	if introspectionModelFlag != "" {
		cfg.IntrospectionAgent.Model = introspectionModelFlag
		log.Infof("sim: introspection model → %s", introspectionModelFlag)
	}
	if classifierModelFlag != "" {
		cfg.Classifier.Model = classifierModelFlag
		log.Infof("sim: classifier model → %s", classifierModelFlag)
	}
	if embedModelFlag != "" {
		cfg.Embed.Model = embedModelFlag
		log.Infof("sim: embed model → %s", embedModelFlag)
	}
	if embedBaseURLFlag != "" {
		cfg.Embed.BaseURL = embedBaseURLFlag
	}
	if embedAPIKeyFlag != "" {
		cfg.Embed.APIKey = embedAPIKeyFlag
	} else if embedBaseURLFlag != "" && cfg.Embed.APIKey == "" {
		cfg.Embed.APIKey = cfg.OpenRouter.APIKey
	}
	if embedDimensionFlag > 0 {
		cfg.Embed.Dimension = embedDimensionFlag
	}

	if fallbackModelFlag != "" {
		log.Infof("sim: fallback model → %s", fallbackModelFlag)
		setFB := func(fb **config.FallbackConfig) {
			if *fb == nil {
				*fb = &config.FallbackConfig{Temperature: 0.3, MaxTokens: 512}
			}
			(*fb).Model = fallbackModelFlag
		}
		setFB(&cfg.Chat.Fallback)
		setFB(&cfg.Driver.Fallback)
		setFB(&cfg.MemoryAgent.Fallback)
		setFB(&cfg.MoodAgent.Fallback)
	}
	if fallbackVisionModelFlag != "" {
		if cfg.Vision.Fallback == nil {
			cfg.Vision.Fallback = &config.FallbackConfig{Temperature: 0.3, MaxTokens: 512}
		}
		cfg.Vision.Fallback.Model = fallbackVisionModelFlag
	}
	if disableReasoningFlag {
		log.Infof("sim: reasoning disabled for hybrid models")
	}
	if directReplyFlag {
		cfg.Driver.DirectReply = true
		simOnly := false
		cfg.Driver.DirectReplySimOnly = &simOnly
		log.Infof("sim: direct reply mode enabled — driver writes reply text directly")
	}
	if noFastPathFlag {
		cfg.Driver.FastPath = false
		log.Infof("sim: fast-path classifier disabled (--no-fast-path)")
	} else if fastPathFlag {
		cfg.Driver.FastPath = true
		log.Infof("sim: fast-path classifier enabled — simple messages skip driver agent")
	}
}

// generateSimReport builds a markdown report from sim results and the
// store's post-run state, writes it to sims/results/, and returns the path.
func generateSimReport(cfg *config.Config, s suite, results []gateway.SimResult, store memory.Store, elapsed time.Duration) (string, error) {
	var b strings.Builder

	// --- Header ---
	totalCost := 0.0
	totalTools := 0
	for _, r := range results {
		totalCost += r.Cost
		totalTools += r.ToolCalls
	}

	fmt.Fprintf(&b, "# Simulation Report: %s\n\n", s.Name)
	if s.Description != "" {
		fmt.Fprintf(&b, "> %s\n\n", s.Description)
	}
	fmt.Fprintf(&b, "**Date:** %s | **Duration:** %s | **Cost:** $%.4f | **Turns:** %d\n\n",
		time.Now().Format("2006-01-02 15:04"),
		elapsed.Round(time.Second),
		totalCost, len(results))

	// Model table
	modelOrNone := func(m string) string {
		if m == "" {
			return "(none)"
		}
		return m
	}
	fmt.Fprintf(&b, "| Role | Model |\n|------|-------|\n")
	fmt.Fprintf(&b, "| Chat | %s |\n", modelOrNone(cfg.Chat.Model))
	fmt.Fprintf(&b, "| Driver | %s |\n", modelOrNone(cfg.Driver.Model))
	fmt.Fprintf(&b, "| Memory | %s |\n", modelOrNone(cfg.MemoryAgent.Model))
	fmt.Fprintf(&b, "| Mood | %s |\n", modelOrNone(cfg.MoodAgent.Model))
	fmt.Fprintf(&b, "| Classifier | %s |\n", modelOrNone(cfg.Classifier.Model))
	fmt.Fprintf(&b, "| Vision | %s |\n", modelOrNone(cfg.Vision.Model))
	fmt.Fprintf(&b, "| Introspection | %s |\n", modelOrNone(cfg.IntrospectionAgent.Model))
	fmt.Fprintf(&b, "| Embed | %s |\n\n", modelOrNone(cfg.Embed.Model))

	// --- Conversation ---
	b.WriteString("## Conversation\n\n")
	for i, r := range results {
		if r.Error != nil {
			fmt.Fprintf(&b, "### Turn %d *(ERROR)*\n", i+1)
			fmt.Fprintf(&b, "**%s:** %s\n\n", cfg.Identity.User, r.Input)
			fmt.Fprintf(&b, "**Error:** %v\n\n---\n\n", r.Error)
			continue
		}

		fmt.Fprintf(&b, "### Turn %d *(%.1fs, $%.4f, %d tools)*\n",
			i+1, r.Duration.Seconds(), r.Cost, r.ToolCalls)
		fmt.Fprintf(&b, "**%s:** %s\n\n", cfg.Identity.User, r.Input)
		fmt.Fprintf(&b, "**%s:** %s\n\n", cfg.Identity.Her, r.Reply)

		if r.FollowUpReply != "" {
			fmt.Fprintf(&b, "**%s** *(follow-up):* %s\n\n", cfg.Identity.Her, r.FollowUpReply)
		}

		// Memory saves
		if len(r.MemoriesSaved) > 0 {
			fmt.Fprintf(&b, "> 🧩 **memory:** saved %d\n", len(r.MemoriesSaved))
			for _, m := range r.MemoriesSaved {
				fmt.Fprintf(&b, "> - %s\n", m)
			}
			b.WriteString("\n")
		}

		// Mood verdict
		if r.MoodVerdict != "" {
			labels := strings.Join(r.MoodLabels, ", ")
			if r.MoodValence > 0 {
				fmt.Fprintf(&b, "> 🎭 **mood:** %s v=%d [%s]\n\n", r.MoodVerdict, r.MoodValence, labels)
			} else {
				fmt.Fprintf(&b, "> 🎭 **mood:** %s\n\n", r.MoodVerdict)
			}
		}

		// Introspection
		if len(r.IntrospectionSaved) > 0 {
			fmt.Fprintf(&b, "> 🪡 **introspection:** saved %d\n", len(r.IntrospectionSaved))
			for _, m := range r.IntrospectionSaved {
				fmt.Fprintf(&b, "> - %s\n", m)
			}
			b.WriteString("\n")
		}

		// Tool log (collapsible)
		if len(r.ToolLog) > 0 {
			b.WriteString("<details><summary>Tool trace</summary>\n\n```\n")
			for _, line := range r.ToolLog {
				fmt.Fprintf(&b, "%s\n", line)
			}
			b.WriteString("```\n</details>\n\n")
		}

		b.WriteString("---\n\n")
	}

	// --- Final Memories ---
	b.WriteString("## Memories (post-run)\n\n")
	memories, err := store.AllActiveMemories()
	if err != nil {
		fmt.Fprintf(&b, "*Error loading memories: %v*\n\n", err)
	} else if len(memories) == 0 {
		b.WriteString("*No memories saved.*\n\n")
	} else {
		fmt.Fprintf(&b, "**%d active memories:**\n\n", len(memories))
		for _, m := range memories {
			subject := "user"
			if m.Subject != "" {
				subject = m.Subject
			}
			superseded := ""
			if m.SupersededBy > 0 {
				superseded = fmt.Sprintf(" *(superseded by #%d)*", m.SupersededBy)
			}
			fmt.Fprintf(&b, "- **#%d** [%s] %s%s\n", m.ID, subject, m.Content, superseded)
		}
		b.WriteString("\n")
	}

	// --- Mood Entries ---
	b.WriteString("## Mood Entries\n\n")
	moods, err := store.RecentMoodEntries(memory.MoodKindMomentary, 50)
	if err != nil {
		fmt.Fprintf(&b, "*Error loading mood entries: %v*\n\n", err)
	} else if len(moods) == 0 {
		b.WriteString("*No mood entries.*\n\n")
	} else {
		fmt.Fprintf(&b, "| # | Valence | Labels | Confidence | Source |\n|---|---------|--------|------------|--------|\n")
		for i, m := range moods {
			fmt.Fprintf(&b, "| %d | %d | %s | %.2f | %s |\n",
				i+1, m.Valence, m.Labels, m.Confidence, m.Source)
		}
		b.WriteString("\n")
	}

	// --- DB-backed sections (via SQLiteStore escape hatch) ---
	sqlStore, hasSQLStore := store.(*memory.SQLiteStore)
	if hasSQLStore {
		writeGWClassifierSection(&b, sqlStore)
		writeGWInboxSection(&b, sqlStore)
		writeGWCostBreakdownSection(&b, sqlStore)
		writeGWFallbackSection(&b, sqlStore)
	}

	// --- Dream Audit ---
	b.WriteString("## Dream Audit\n\n")
	audits, err := store.RecentDreamAudits(50)
	if err != nil {
		fmt.Fprintf(&b, "*Error loading dream audits: %v*\n\n", err)
	} else if len(audits) == 0 {
		b.WriteString("*No dream operations.*\n\n")
	} else {
		opIcons := map[string]string{"merge": "🔀", "expire": "🗑", "promote": "⬆️", "split": "✂️", "create": "✨"}
		for _, a := range audits {
			icon := opIcons[a.Operation]
			if icon == "" {
				icon = "🔧"
			}
			fmt.Fprintf(&b, "- %s **%s** #%v → #%d: %s\n",
				icon, a.Operation, a.SourceIDs, a.ResultID, a.Reason)
			if a.BeforeText != "" {
				fmt.Fprintf(&b, "  - before: %s\n", truncSimText(a.BeforeText, 100))
			}
			if a.AfterText != "" {
				fmt.Fprintf(&b, "  - after: %s\n", truncSimText(a.AfterText, 100))
			}
		}
		b.WriteString("\n")
	}

	// --- Cost Summary ---
	b.WriteString("## Cost Summary\n\n")
	fmt.Fprintf(&b, "| Metric | Value |\n|--------|-------|\n")
	fmt.Fprintf(&b, "| Total cost | $%.4f |\n", totalCost)
	fmt.Fprintf(&b, "| Total tools | %d |\n", totalTools)
	fmt.Fprintf(&b, "| Turns | %d |\n", len(results))
	if len(results) > 0 {
		fmt.Fprintf(&b, "| Avg cost/turn | $%.4f |\n", totalCost/float64(len(results)))
	}
	fmt.Fprintf(&b, "| Duration | %s |\n\n", elapsed.Round(time.Second))

	// --- Write to file ---
	if err := os.MkdirAll("sims/results", 0o755); err != nil {
		return "", fmt.Errorf("creating results dir: %w", err)
	}

	slug := strings.ReplaceAll(strings.ToLower(s.Name), " ", "-")
	filename := fmt.Sprintf("sims/results/%s-%s.md",
		slug, time.Now().Format("20060102-150405"))
	if err := os.WriteFile(filename, []byte(b.String()), 0o644); err != nil {
		return "", fmt.Errorf("writing report: %w", err)
	}

	return filename, nil
}

// --- Report section writers (query store via SQLiteStore.DB() escape hatch) ---

func writeGWClassifierSection(b *strings.Builder, s *memory.SQLiteStore) {
	b.WriteString("## Classifier Verdicts\n\n")

	rows, err := s.DB().Query(
		`SELECT write_type, verdict, content, reason, rewrite
		 FROM classifier_log ORDER BY id`)
	if err != nil {
		fmt.Fprintf(b, "*Error loading classifier log: %v*\n\n", err)
		return
	}
	defer rows.Close()

	var count int
	for rows.Next() {
		var writeType, verdict, content, reason, rewrite string
		if err := rows.Scan(&writeType, &verdict, &content, &reason, &rewrite); err != nil {
			continue
		}
		if count == 0 {
			fmt.Fprintf(b, "| Type | Verdict | Content | Reason |\n|------|---------|---------|--------|\n")
		}
		content = truncSimText(content, 50)
		reason = truncSimText(reason, 50)
		fmt.Fprintf(b, "| %s | %s | %s | %s |\n", writeType, verdict, content, reason)
		count++
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(b, "\n*Error iterating classifier log: %v*\n", err)
	}
	if count == 0 {
		b.WriteString("*No classifier verdicts.*\n")
	}
	b.WriteString("\n")
}

func writeGWInboxSection(b *strings.Builder, s *memory.SQLiteStore) {
	b.WriteString("## Inter-Agent Messages\n\n")

	rows, err := s.DB().Query(
		`SELECT sender, recipient, msg_type, payload, status
		 FROM inbox ORDER BY id`)
	if err != nil {
		fmt.Fprintf(b, "*Error loading inbox: %v*\n\n", err)
		return
	}
	defer rows.Close()

	var count int
	for rows.Next() {
		var sender, recipient, msgType, payload, status string
		if err := rows.Scan(&sender, &recipient, &msgType, &payload, &status); err != nil {
			continue
		}
		if count == 0 {
			fmt.Fprintf(b, "| Sender | Recipient | Type | Status | Payload |\n|--------|-----------|------|--------|---------|\n")
		}
		payload = truncSimText(payload, 60)
		fmt.Fprintf(b, "| %s | %s | %s | %s | %s |\n", sender, recipient, msgType, status, payload)
		count++
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(b, "\n*Error iterating inbox: %v*\n", err)
	}
	if count == 0 {
		b.WriteString("*No inter-agent messages.*\n")
	}
	b.WriteString("\n")
}

func writeGWCostBreakdownSection(b *strings.Builder, s *memory.SQLiteStore) {
	b.WriteString("## Cost Breakdown by Role\n\n")

	rows, err := s.DB().Query(
		`SELECT agent_role,
		        COUNT(*) as calls,
		        SUM(prompt_tokens) as prompt,
		        SUM(completion_tokens) as completion,
		        SUM(cost_usd) as cost,
		        SUM(CASE WHEN is_fallback = 1 THEN 1 ELSE 0 END) as fallbacks
		 FROM metrics
		 GROUP BY agent_role
		 ORDER BY cost DESC`)
	if err != nil {
		fmt.Fprintf(b, "*Error loading metrics: %v*\n\n", err)
		return
	}
	defer rows.Close()

	var count int
	for rows.Next() {
		var role string
		var calls, prompt, completion, fallbacks int
		var cost float64
		if err := rows.Scan(&role, &calls, &prompt, &completion, &cost, &fallbacks); err != nil {
			continue
		}
		if count == 0 {
			fmt.Fprintf(b, "| Role | Calls | Prompt | Completion | Cost | Fallbacks |\n|------|-------|--------|------------|------|-----------|\n")
		}
		fb := ""
		if fallbacks > 0 {
			fb = fmt.Sprintf("%d", fallbacks)
		}
		fmt.Fprintf(b, "| %s | %d | %d | %d | $%.4f | %s |\n", role, calls, prompt, completion, cost, fb)
		count++
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(b, "\n*Error iterating metrics: %v*\n", err)
	}
	if count == 0 {
		b.WriteString("*No metrics recorded.*\n")
	}
	b.WriteString("\n")
}

func writeGWFallbackSection(b *strings.Builder, s *memory.SQLiteStore) {
	var totalCalls, fallbackCalls int
	var fallbackCost float64

	row := s.DB().QueryRow(
		`SELECT COUNT(*), SUM(CASE WHEN is_fallback = 1 THEN 1 ELSE 0 END),
		        COALESCE(SUM(CASE WHEN is_fallback = 1 THEN cost_usd ELSE 0 END), 0)
		 FROM metrics`)
	if err := row.Scan(&totalCalls, &fallbackCalls, &fallbackCost); err != nil || fallbackCalls == 0 {
		return
	}

	pct := float64(fallbackCalls) / float64(totalCalls) * 100
	b.WriteString("## Fallback Events\n\n")
	fmt.Fprintf(b, "**%d of %d calls (%.0f%%) used fallback models** — $%.4f in fallback costs\n\n",
		fallbackCalls, totalCalls, pct, fallbackCost)

	rows, err := s.DB().Query(
		`SELECT model, COUNT(*), SUM(cost_usd)
		 FROM metrics WHERE is_fallback = 1
		 GROUP BY model ORDER BY COUNT(*) DESC`)
	if err != nil {
		return
	}
	defer rows.Close()

	fmt.Fprintf(b, "| Fallback Model | Calls | Cost |\n|----------------|-------|------|\n")
	for rows.Next() {
		var model string
		var calls int
		var cost float64
		if err := rows.Scan(&model, &calls, &cost); err != nil {
			continue
		}
		fmt.Fprintf(b, "| %s | %d | $%.4f |\n", model, calls, cost)
	}
	if err := rows.Err(); err != nil {
		fmt.Fprintf(b, "\n*Error iterating fallback metrics: %v*\n", err)
	}
	b.WriteString("\n")
}

// seedSimDB pre-populates the sim database with cards, memories, self
// memories, and persona before the gateway starts. This ensures the
// agent has context to work with from the first message.
func seedSimDB(store memory.Store, embedClient *embed.Client, cfg *config.Config, s suite) (personaCleanup func(), err error) {
	// --- Seed memory cards ---
	if s.SeedCards {
		log.Infof("sim: seeding memory cards...")
		// Type-assert to access DB() — the documented escape hatch for
		// infrastructure code that needs raw SQL.
		sqlStore, ok := store.(*memory.SQLiteStore)
		if !ok {
			return nil, fmt.Errorf("card seeding requires SQLiteStore")
		}
		seedCardSQL := `INSERT OR IGNORE INTO memory_cards (topic_slug, name, subject, protected) VALUES (?, ?, ?, ?)`
		cards := []struct{ slug, name, subject string }{
			{"identity", "Identity", "user"},
			{"health", "Health", "user"},
			{"financial", "Financial", "user"},
			{"family", "Family", "user"},
			{"relationships", "Relationships", "user"},
			{"work", "Work & Career", "user"},
			{"interests", "Interests", "user"},
			{"projects", "Projects", "user"},
			{"routines", "Routines", "user"},
			{"my-identity", "My Identity", "self"},
			{"my-emotions", "My Emotions", "self"},
			{"my-communication", "My Communication", "self"},
			{"my-relationship", "My Relationship", "self"},
			{"my-growth", "My Growth", "self"},
		}
		for _, c := range cards {
			if _, err := sqlStore.DB().Exec(seedCardSQL, c.slug, c.name, c.subject, 1); err != nil {
				log.Warnf("sim: seed card %q failed: %v", c.slug, err)
			}
		}
	}

	// --- Resolve card slug → ID ---
	resolveCardID := func(slug string) int64 {
		if slug == "" {
			return 0
		}
		card, err := store.GetCard(slug)
		if err != nil || card == nil {
			return 0
		}
		return card.ID
	}

	// --- Seed user memories ---
	if len(s.SeedMemories) > 0 {
		log.Infof("sim: seeding %d memories...", len(s.SeedMemories))
		for _, m := range s.SeedMemories {
			var vec []float32
			if embedClient != nil {
				v, err := embedClient.Embed(m.Content)
				if err != nil {
					log.Warnf("sim: seed embed failed: %v", err)
				} else {
					vec = v
				}
			}
			cardID := resolveCardID(m.Card)
			id, err := store.SaveMemory(m.Content, "", "user", 0, 5, vec, vec, "", "", cardID)
			if err != nil {
				log.Errorf("sim: seed memory failed: %v", err)
				continue
			}
			if len(vec) > 0 {
				_ = store.AutoLinkMemory(id, vec)
			}
			log.Infof("sim:   seeded #%d: %s", id, truncSimText(m.Content, 60))
		}
	}

	// --- Seed self memories ---
	if len(s.SeedSelfMemories) > 0 {
		log.Infof("sim: seeding %d self memories...", len(s.SeedSelfMemories))
		for _, m := range s.SeedSelfMemories {
			var vec []float32
			if embedClient != nil {
				v, err := embedClient.Embed(m.Content)
				if err != nil {
					log.Warnf("sim: seed embed failed: %v", err)
				} else {
					vec = v
				}
			}
			cardID := resolveCardID(m.Card)
			id, err := store.SaveMemory(m.Content, "", "self", 0, 5, vec, vec, "", "", cardID)
			if err != nil {
				log.Errorf("sim: seed self memory failed: %v", err)
				continue
			}
			if len(vec) > 0 {
				_ = store.AutoLinkMemory(id, vec)
			}
			log.Infof("sim:   seeded self #%d: %s", id, truncSimText(m.Content, 60))
		}
	}

	// --- Seed calendar events ---
	if len(s.SeedCalendarEvents) > 0 {
		log.Infof("sim: seeding %d calendar events...", len(s.SeedCalendarEvents))
		for _, seed := range s.SeedCalendarEvents {
			cal := seed.Calendar
			if cal == "" {
				cal = cfg.Calendar.DefaultCalendar
			}
			dbID, seedErr := store.InsertCalendarEvent(
				seed.Title, seed.Start, seed.End,
				seed.Location, seed.Notes, cal,
				seed.ID, seed.Job,
			)
			if seedErr != nil {
				log.Errorf("sim: seed calendar event failed: %v", seedErr)
				continue
			}
			if seed.Job != "" {
				log.Infof("sim:   seeded cal #%d: %s [%s shift] (event %s)", dbID, seed.Title, seed.Job, seed.ID)
			} else {
				log.Infof("sim:   seeded cal #%d: %s (event %s)", dbID, seed.Title, seed.ID)
			}
		}
	}

	// --- Seed persona ---
	if s.SeedPersona != "" {
		tmpPersona, err := os.CreateTemp("", "her-sim-persona-*.md")
		if err != nil {
			return nil, fmt.Errorf("creating temp persona file: %w", err)
		}
		if _, err := tmpPersona.WriteString(s.SeedPersona); err != nil {
			tmpPersona.Close()
			os.Remove(tmpPersona.Name())
			return nil, fmt.Errorf("writing seed persona: %w", err)
		}
		tmpPersona.Close()
		cfg.Persona.PersonaFile = tmpPersona.Name()
		// personaCleanup is returned so the caller can defer it.
		// We can't defer here because seedSimDB returns before the
		// gateway finishes using the file.
		personaCleanup = func() { os.Remove(tmpPersona.Name()) }
		log.Infof("sim: seeded persona (%d bytes)", len(s.SeedPersona))
	}

	return personaCleanup, nil
}
