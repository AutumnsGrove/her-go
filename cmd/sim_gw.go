package cmd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"her/config"
	"her/embed"
	"her/gateway"
	"her/llm"
	"her/memory"
	"her/search"
	"her/tui"

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
	// --suite / -s is the only required flag. The rest of the sim flags
	// (--limit, --delay, model overrides) live on sim-legacy for now —
	// they can be ported here if needed later.
	simGWCmd.Flags().StringVarP(&suiteFlag, "suite", "s", "", "path to suite YAML file (required)")
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
	// --- Load config ---
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	if cfg.OpenRouter.APIKey == "" {
		return fmt.Errorf("LLM API key is required — set OPENROUTER_API_KEY or fill in config.yaml")
	}

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
	defer os.RemoveAll(tmpDir) // runs after runSimGW returns, cleans up DB

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

	if err := seedSimDB(store, embedClient, cfg, s); err != nil {
		return fmt.Errorf("seeding sim database: %w", err)
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

	driverLLM := llm.NewClient(cfg.OpenRouter.BaseURL, cfg.OpenRouter.APIKey,
		cfg.Driver.Model, cfg.Driver.Temperature, cfg.Driver.MaxTokens)
	if cfg.Driver.Timeout > 0 {
		driverLLM.WithTimeout(time.Duration(cfg.Driver.Timeout) * time.Second)
	}
	if cfg.Driver.Fallback != nil {
		driverLLM.WithFallback(cfg.Driver.Fallback.Model, cfg.Driver.Fallback.Temperature, cfg.Driver.Fallback.MaxTokens)
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
		ConfigPath:       cfgFile,
		// VoiceClient and TTSClient intentionally nil — no audio in sim mode.
	}

	// --- Convert suite messages to gateway.SimMessage ---
	// simMessage (cmd package) → gateway.SimMessage (gateway package).
	// Same shape, different types — the boundary between CLI and gateway.
	gwMessages := make([]gateway.SimMessage, len(s.Messages))
	for i, m := range s.Messages {
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
		cancel()              // sim finished normally — signal gateway to stop
		gatewayErr = <-gwErrCh // drain; gw.Run returns nil after ctx cancel
	case gatewayErr = <-gwErrCh:
		cancel() // gateway exited before sim finished — cancel context anyway
		if gatewayErr != nil {
			return fmt.Errorf("gateway exited unexpectedly: %w", gatewayErr)
		}
	}
	_ = gatewayErr

	// --- Print results ---
	results := sa.Results()
	printSimResults(s.Name, results)

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

// seedSimDB pre-populates the sim database with cards, memories, self
// memories, and persona before the gateway starts. This ensures the
// agent has context to work with from the first message.
func seedSimDB(store memory.Store, embedClient *embed.Client, cfg *config.Config, s suite) error {
	// --- Seed memory cards ---
	if s.SeedCards {
		log.Infof("sim: seeding memory cards...")
		// Type-assert to access DB() — the documented escape hatch for
		// infrastructure code that needs raw SQL.
		sqlStore, ok := store.(*memory.SQLiteStore)
		if !ok {
			return fmt.Errorf("card seeding requires SQLiteStore")
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

	// --- Seed persona ---
	if s.SeedPersona != "" {
		tmpPersona, err := os.CreateTemp("", "her-sim-persona-*.md")
		if err != nil {
			return fmt.Errorf("creating temp persona file: %w", err)
		}
		if _, err := tmpPersona.WriteString(s.SeedPersona); err != nil {
			tmpPersona.Close()
			return fmt.Errorf("writing seed persona: %w", err)
		}
		tmpPersona.Close()
		cfg.Persona.PersonaFile = tmpPersona.Name()
		log.Infof("sim: seeded persona (%d bytes)", len(s.SeedPersona))
	}

	return nil
}
