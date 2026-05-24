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

	// Inject a single sim adapter into the gateway config. We set this
	// programmatically rather than reading from config.yaml so sim runs
	// never accidentally share memory with the production database.
	cfg.Gateway.Adapters = []config.AdapterConfig{{
		Name: "sim",
		Type: "sim",
		DB:   tmpDBPath,
	}}

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

	// --- Embed + search clients ---
	embedClient := embed.NewClient(cfg.Embed.BaseURL, cfg.Embed.Model, cfg.Embed.APIKey, cfg.Embed.Dimension)
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

// printSimResults renders a simple table of sim turn results to stdout.
// No TUI, no color — just clear text output for CI and shell usage.
func printSimResults(suiteName string, results []gateway.SimResult) {
	passed := 0
	failed := 0
	for _, r := range results {
		if r.Error == nil {
			passed++
		} else {
			failed++
		}
	}

	fmt.Printf("\n=== Sim Results: %s ===\n", suiteName)
	fmt.Printf("Turns: %d  Passed: %d  Failed: %d\n\n", len(results), passed, failed)

	for i, r := range results {
		status := "OK"
		if r.Error != nil {
			status = "ERR"
		}
		fmt.Printf("[%d/%d] %-4s  %s\n", i+1, len(results), status, truncSimText(r.Input, 60))
		if r.Error != nil {
			fmt.Printf("         error: %v\n", r.Error)
		} else {
			fmt.Printf("         reply (%s): %s\n", r.Duration.Round(time.Millisecond), truncSimText(r.Reply, 80))
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
