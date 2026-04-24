package cmd

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"her/agent"
	"her/calendar"
	"her/compact"
	"her/config"
	"her/embed"
	"her/llm"
	"her/memory"
	"her/mood"
	"her/persona"
	"her/scheduler"
	"her/scrub"
	"her/search"
	"her/turn"

	// golang-migrate provides forward-only database migrations, same as
	// memory.Store uses for her.db. The file source reads .up.sql files
	// from migrations/sim/ and the sqlite3 database driver handles the
	// connection. Both underscore imports register their drivers via init().
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/sqlite3"
	_ "github.com/golang-migrate/migrate/v4/source/file"

	// Underscore import: registers the SQLite driver with database/sql.
	// We need this for the sim.db connection (separate from memory.Store
	// which handles its own driver registration via sqlite-vec).
	_ "github.com/mattn/go-sqlite3"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

// fallbackSimAgentModel is used in sim reports when cfg.Driver.Model is empty.
// The real default lives in config.yaml.example; this is a last-resort label
// for display purposes only, not used to make actual LLM calls.
const fallbackSimAgentModel = "liquid/lfm-2.5-1.2b-instruct:free"

// suiteFlag holds the path to the suite YAML file, set via --suite / -s.
var suiteFlag string

// limitFlag caps the number of messages to send. 0 = all messages.
// Useful for testing with `--limit 1` to just send the first message
// without burning through all your tokens.
var limitFlag int

// delayFlag is the pause between turns in seconds. Defaults to 5s.
// In real usage, messages are minutes apart so rate limits aren't an issue.
// In sim mode we fire back-to-back, which can hit free-tier rate limits
// on the driver agent model. A few seconds between turns fixes this.
var delayFlag int

// driverModelFlag overrides the driver agent model from config.yaml for this run.
// Useful for comparing different models without editing the config file.
// Example: --driver-model "qwen/qwen3-235b-a22b-2507"
var driverModelFlag string

// embedModelFlag overrides the embedding model from config.yaml for this run.
// Each embedding model lives in its own vector space, so swapping models
// means all embeddings in the run use the new model's geometry. The sim's
// clean-room temp DB ensures no stale vectors from a different model leak in.
// Example: --embed-model "voyage-4-nano"
var embedModelFlag string

// embedBaseURLFlag overrides the embedding API base URL. Needed when
// switching between local (LM Studio) and remote (OpenRouter) embeddings.
// Example: --embed-base-url "https://openrouter.ai/api/v1"
var embedBaseURLFlag string

// embedAPIKeyFlag provides an API key for remote embedding APIs.
// Not needed for local servers. If empty and a remote URL is detected,
// falls back to the LLM API key (since OpenRouter uses the same key for both).
var embedAPIKeyFlag string

// embedDimensionFlag overrides the embedding dimension from config.yaml.
// Must match the output dimension of the model specified by --embed-model.
// Different models produce different-sized vectors (e.g., nomic = 768,
// OpenAI = 1536), and the sqlite-vec virtual table needs the right size
// at creation time.
var embedDimensionFlag int

// memoryModelFlag overrides the memory agent model for this run.
// Example: --memory-model "qwen/qwen3-235b-a22b-2507"
var memoryModelFlag string

// chatModelFlag overrides the chat (reply) model for this run.
// Example: --chat-model "anthropic/claude-haiku-4.5"
var chatModelFlag string

// chatProviderFlag pins the chat model to a specific OpenRouter inference
// provider. Comma-separated list tried in order — first available wins.
// Useful when a model is hosted by multiple providers at different speeds:
// e.g., the memory agent model hosted on Groq is much faster than on OpenRouter's default routing.
// Example: --chat-provider "Groq" or --chat-provider "Groq,Together"
var chatProviderFlag string

// classifierModelFlag overrides the classifier model for this run.
// Useful for A/B testing different models as the safety/quality gate.
// Example: --classifier-model "google/gemini-2.5-flash-lite"
var classifierModelFlag string

// fallbackModelFlag overrides the fallback model for chat, agent, memory,
// and mood roles. When free-tier primary models hit rate limits (16 req/min
// on OpenRouter), the system silently falls back to this model. Defaults
// to Claude Haiku in config — this flag lets you test cheaper alternatives
// like Gemini Flash Lite without editing config.yaml.
// Example: --fallback-model "google/gemini-2.5-flash-lite"
var fallbackModelFlag string

// fallbackVisionModelFlag overrides the fallback model for the vision role
// only, kept separate from --fallback-model because vision fallbacks have
// different requirements (must support multi-modal input).
// Example: --fallback-vision-model "google/gemini-2.5-flash-lite"
var fallbackVisionModelFlag string

// disableReasoningFlag disables reasoning mode for hybrid models that support
// both reasoning and non-reasoning modes (e.g., Qwen3.6, DeepSeek V3.2).
// Pure reasoning models (DeepSeek R1, V4) will ignore this flag — they always reason.
// Example: --disable-reasoning
var disableReasoningFlag bool

// simCmd defines the "her sim" subcommand. Cobra commands are just structs
// with metadata + a RunE function. RunE returns an error (vs Run which doesn't),
// so Cobra can print it nicely and set the exit code. Same idea as argparse
// subcommands in Python, but the wiring is struct-based instead of method calls.
var simCmd = &cobra.Command{
	Use:   "sim",
	Short: "Run a scripted conversation simulation",
	Long: `Runs a suite of scripted messages through the real chatbot pipeline
in a clean-room environment. Results are saved to sims/sim.db and a
Markdown report is generated in sims/results/.

Example:
  her sim --suite sims/getting-to-know-you.yaml`,
	RunE: runSim,
}

// init registers the sim command with the root command. In Go, init() functions
// run automatically when the package loads — like Python's module-level code,
// but guaranteed to run before main(). Each file can have its own init().
func init() {
	simCmd.Flags().StringVarP(&suiteFlag, "suite", "s", "", "path to suite YAML file (required)")
	simCmd.Flags().IntVarP(&limitFlag, "limit", "n", 0, "max messages to send (0 = all)")
	simCmd.Flags().IntVarP(&delayFlag, "delay", "d", 1, "seconds to wait between turns")
	simCmd.Flags().StringVar(&driverModelFlag, "driver-model", "", "override driver agent model for this run (e.g., qwen/qwen3-235b-a22b-2507)")
	simCmd.Flags().StringVar(&embedModelFlag, "embed-model", "", "override embedding model for this run (e.g., voyage-4-nano)")
	simCmd.Flags().StringVar(&embedBaseURLFlag, "embed-base-url", "", "override embedding API base URL (e.g., https://openrouter.ai/api/v1)")
	simCmd.Flags().StringVar(&embedAPIKeyFlag, "embed-api-key", "", "API key for remote embedding APIs (defaults to LLM API key if empty)")
	simCmd.Flags().IntVar(&embedDimensionFlag, "embed-dimension", 0, "override embedding dimension (must match --embed-model output size)")
	simCmd.Flags().StringVar(&memoryModelFlag, "memory-model", "", "override memory agent model for this run (e.g., qwen/qwen3-235b-a22b-2507)")
	simCmd.Flags().StringVar(&chatModelFlag, "chat-model", "", "override chat (reply) model for this run (e.g., anthropic/claude-haiku-4.5)")
	simCmd.Flags().StringVar(&chatProviderFlag, "chat-provider", "", "pin chat model to OpenRouter provider(s), comma-separated (e.g., \"Groq\" or \"Groq,Together\")")
	simCmd.Flags().StringVar(&classifierModelFlag, "classifier-model", "", "override classifier model for this run (e.g., google/gemini-2.5-flash-lite)")
	simCmd.Flags().StringVar(&fallbackModelFlag, "fallback-model", "", "override fallback model for chat/agent/memory/mood (e.g., google/gemini-2.5-flash-lite)")
	simCmd.Flags().StringVar(&fallbackVisionModelFlag, "fallback-vision-model", "", "override fallback model for vision only (must support multi-modal)")
	simCmd.Flags().BoolVar(&disableReasoningFlag, "disable-reasoning", false, "disable reasoning mode for hybrid models (Qwen3.6, DeepSeek V3.2)")
	// MarkFlagRequired makes Cobra error out if --suite is missing,
	// so we don't have to check it ourselves in runSim.
	simCmd.MarkFlagRequired("suite")
	rootCmd.AddCommand(simCmd)
}

// --------------------------------------------------------------------------
// Suite YAML structure
// --------------------------------------------------------------------------

// suite represents the YAML file that defines a scripted conversation.
// The struct tags tell the YAML parser which keys to look for.
type suite struct {
	Name               string              `yaml:"name"`
	Description        string              `yaml:"description"`
	Tags               []string            `yaml:"tags"`
	Messages           []simMessage        `yaml:"messages"`
	SeedMemories       []string            `yaml:"seed_memories"`        // pre-populated before message loop (with embeddings)
	SeedCalendarEvents []SeedCalendarEvent `yaml:"seed_calendar_events"` // calendar events (FakeBridge + DB)
	CompactAfter       int                 `yaml:"compact_after"`        // force compaction after turn N (0 = disabled)
	RunDream           bool                `yaml:"run_dream"`            // run a full dream cycle after all messages complete
	RunRollup          bool                `yaml:"run_rollup"`           // force the daily mood rollup after all messages complete
}

// SeedCalendarEvent represents a calendar event to populate in sims.
// Stored in both the DB (SQLite source of truth) and FakeBridge (for EventKit simulation).
// Can represent any calendar event — meetings, shifts, appointments, etc.
type SeedCalendarEvent struct {
	ID       string `yaml:"id"`       // EventKit-style identifier (e.g., "SEED-001")
	Title    string `yaml:"title"`    // Event title
	Start    string `yaml:"start"`    // ISO8601 with timezone
	End      string `yaml:"end"`      // ISO8601 with timezone
	Location string `yaml:"location,omitempty"`
	Notes    string `yaml:"notes,omitempty"` // Can include shift metadata like "position: Bake\ntrainer: Mike\n..."
	Calendar string `yaml:"calendar,omitempty"` // defaults to config.Calendar.DefaultCalendar
	Job      string `yaml:"job,omitempty"`      // Job name (e.g., "Panera") — marks this as a shift event
}

// simMessage represents a single message in a sim suite. It can be either
// a plain text string or a structured message with an image path. The custom
// UnmarshalYAML lets both forms coexist in the same YAML list:
//
//	messages:
//	  - "hey, look at this photo"          # plain string → Text only
//	  - image: sims/assets/sunset.jpg      # image-only → Image path only
//	  - text: "what do you think?"          # explicit text form (also works)
//	    image: sims/assets/sunset.jpg       # can combine text + image
//
// This is similar to Python's Union[str, dict] pattern, but in Go we use
// a struct with a custom unmarshal method. The YAML library calls
// UnmarshalYAML, which tries string first (fast path), then falls back
// to the struct form.
type simMessage struct {
	Text  string `yaml:"text"`
	Image string `yaml:"image"` // path to local image file (relative to working dir)
}

// UnmarshalYAML implements yaml.Unmarshaler so a simMessage can be decoded
// from either a plain string or a mapping with text/image keys. node.Decode
// is how go-yaml lets you try multiple interpretations of the same YAML node.
func (m *simMessage) UnmarshalYAML(node *yaml.Node) error {
	// Fast path: plain string → text-only message.
	if node.Kind == yaml.ScalarNode {
		m.Text = node.Value
		return nil
	}

	// Slow path: mapping with text/image keys.
	// Use a type alias to avoid infinite recursion — without this,
	// node.Decode(&m) would call UnmarshalYAML again forever.
	type rawMsg simMessage
	var raw rawMsg
	if err := node.Decode(&raw); err != nil {
		return fmt.Errorf("decoding sim message: %w", err)
	}
	m.Text = raw.Text
	m.Image = raw.Image
	return nil
}

// IsImage returns true if this message includes an image attachment.
func (m simMessage) IsImage() bool { return m.Image != "" }

// DisplayText returns the user-visible text for this message. For image-only
// messages this mirrors what Telegram sends: "[User sent a photo]".
func (m simMessage) DisplayText() string {
	if m.Text != "" {
		if m.Image != "" {
			return "[User sent a photo] " + m.Text
		}
		return m.Text
	}
	if m.Image != "" {
		return "[User sent a photo]"
	}
	return ""
}

// simTurnResult captures the outcome of one conversation turn during a
// simulation. Defined at package level (not inside a function) so it can
// be shared between runSim and generateReport. In Go, types defined
// inside a function are scoped to that function — they can't be used as
// parameters elsewhere.
type simTurnResult struct {
	userMsg       string
	botReply      string
	followUpReply string // from EventInboxReady — empty if no background task reported back
	elapsed       time.Duration
}

// simRollupResult captures the output of a forced daily mood rollup
// (run_rollup: true). In production the rollup fires at 21:00 local
// via the scheduler — the sim skips that clock and invokes the
// handler directly so we can verify the aggregation without waiting
// for an actual day to pass.
type simRollupResult struct {
	Ran         bool
	Skipped     bool   // true when the handler decided there was nothing to roll up
	SkipReason  string // human-readable reason for Skipped
	EntryID     int64
	Valence     int
	Labels      []string
	Associations []string
	Note        string
	SummaryText string // what the bot would have sent to the owner chat
	Error       string
}

// simDreamResult captures the output of the dream cycle (run_dream: true)
// so it can be included in the markdown report. Fields are empty strings
// when the dream step didn't run or returned nothing notable.
type simDreamResult struct {
	Ran           bool   // true if the dream cycle executed
	Reflection    string // NightlyReflect output (or "NOTHING_NOTABLE")
	ReflectError  string // non-empty if NightlyReflect failed
	PersonaText   string // new persona.md content after rewrite
	ChangeSummary string // CHANGE_SUMMARY line from GatedRewrite
	Rewritten     bool   // true if persona was actually rewritten
	RewriteError  string // non-empty if GatedRewrite failed
}

// --------------------------------------------------------------------------
// Main command logic
// --------------------------------------------------------------------------

// runSim is the entry point for "her sim --suite path/to/suite.yaml".
// It loads a suite, runs every message through the real agent pipeline
// using a throwaway database, then copies the results to a persistent
// sim.db for later comparison.
func runSim(cmd *cobra.Command, args []string) error {
	startTime := time.Now()

	// ------------------------------------------------------------------
	// 1. Parse the suite YAML
	// ------------------------------------------------------------------

	// os.ReadFile reads an entire file into a byte slice — like Python's
	// open(path).read(). In Go, files return []byte, not strings.
	suiteBytes, err := os.ReadFile(suiteFlag)
	if err != nil {
		return fmt.Errorf("reading suite file: %w", err)
	}

	var s suite
	// yaml.Unmarshal is like json.loads() in Python — it takes raw bytes
	// and fills in a struct. The &s passes a pointer so Unmarshal can
	// modify the struct in place.
	if err := yaml.Unmarshal(suiteBytes, &s); err != nil {
		return fmt.Errorf("parsing suite YAML: %w", err)
	}

	if len(s.Messages) == 0 {
		return fmt.Errorf("suite %q has no messages", s.Name)
	}

	log.Info("simulation starting", "suite", s.Name, "messages", len(s.Messages))
	if s.Description != "" {
		log.Infof("  %s", s.Description)
	}
	if s.CompactAfter > 0 {
		log.Infof("  forced compaction after turn %d", s.CompactAfter)
	}

	// ------------------------------------------------------------------
	// 2. Load config (skip Telegram + LLM key checks)
	// ------------------------------------------------------------------

	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Export config secrets as process-level env vars so skills can
	// find them. Without this, skills like web_search fail because
	// TAVILY_API_KEY isn't in the environment. run.go does this too.
	cfg.ExportEnv()

	// Warn but don't fatal on missing API key — the user might just be
	// testing the sim harness itself.
	if cfg.LLM.APIKey == "" {
		log.Warn("LLM API key not set — agent calls will fail")
	}

	// --driver-model flag overrides the config value. This mutates cfg so
	// both the run logic and report generator see the same model name.
	if driverModelFlag != "" {
		log.Info("Driver agent model overridden via --driver-model", "model", driverModelFlag)
		cfg.Driver.Model = driverModelFlag
	}
	if memoryModelFlag != "" {
		log.Info("Memory agent model overridden via --memory-model", "model", memoryModelFlag)
		cfg.MemoryAgent.Model = memoryModelFlag
	}
	if chatModelFlag != "" {
		log.Info("Chat model overridden via --chat-model", "model", chatModelFlag)
		cfg.Chat.Model = chatModelFlag
	}

	// --embed-* flags override the embedding config. This lets you test
	// remote models (OpenRouter, OpenAI) without touching config.yaml.
	if embedBaseURLFlag != "" {
		log.Info("Embed base URL overridden via --embed-base-url", "url", embedBaseURLFlag)
		cfg.Embed.BaseURL = embedBaseURLFlag
	}
	if embedModelFlag != "" {
		log.Info("Embed model overridden via --embed-model", "model", embedModelFlag)
		cfg.Embed.Model = embedModelFlag
	}
	if embedAPIKeyFlag != "" {
		cfg.Embed.APIKey = embedAPIKeyFlag
		log.Info("Embed API key provided via --embed-api-key")
	} else if embedBaseURLFlag != "" && cfg.Embed.APIKey == "" {
		// If switching to a remote URL but no embed API key is set,
		// fall back to the LLM API key — on OpenRouter, it's the same key.
		cfg.Embed.APIKey = cfg.LLM.APIKey
		log.Info("Embed API key defaulting to LLM API key for remote embeddings")
	}
	if embedDimensionFlag > 0 {
		log.Info("Embed dimension overridden via --embed-dimension", "dimension", embedDimensionFlag)
		cfg.Embed.Dimension = embedDimensionFlag
	}

	// --fallback-model overrides the fallback for chat, agent, memory, and
	// mood roles. This mutates the FallbackConfig on each role — creating
	// one if it didn't exist. Temperature and max_tokens are preserved from
	// the existing config when available, otherwise sensible defaults apply.
	if fallbackModelFlag != "" {
		log.Info("Fallback model overridden via --fallback-model", "model", fallbackModelFlag)

		// Helper: ensure a FallbackConfig exists, then swap the model.
		// Keeps existing temperature/max_tokens so only the model changes.
		setFallback := func(fb **config.FallbackConfig) {
			if *fb == nil {
				*fb = &config.FallbackConfig{Temperature: 0.3, MaxTokens: 512}
			}
			(*fb).Model = fallbackModelFlag
		}

		setFallback(&cfg.Chat.Fallback)
		setFallback(&cfg.Driver.Fallback)
		setFallback(&cfg.MemoryAgent.Fallback)
		setFallback(&cfg.MoodAgent.Fallback)
	}

	// --fallback-vision-model overrides the vision fallback separately.
	// Vision fallbacks need multi-modal support, so they're independent
	// from the general --fallback-model flag.
	if fallbackVisionModelFlag != "" {
		log.Info("Vision fallback model overridden via --fallback-vision-model", "model", fallbackVisionModelFlag)
		if cfg.Vision.Fallback == nil {
			cfg.Vision.Fallback = &config.FallbackConfig{Temperature: 0.3, MaxTokens: 512}
		}
		cfg.Vision.Fallback.Model = fallbackVisionModelFlag
	}

	// ------------------------------------------------------------------
	// 3. Open/create sims/sim.db for persistent results
	// ------------------------------------------------------------------

	// os.MkdirAll is like Python's os.makedirs(exist_ok=True) — creates
	// the directory and all parents, no error if it already exists.
	if err := os.MkdirAll("sims/results", 0o755); err != nil {
		return fmt.Errorf("creating sims directory: %w", err)
	}

	// golang-migrate handles sim.db schema the same way memory.Store
	// handles her.db — file-based SQL migrations applied in order.
	// The "file://" source reads from migrations/sim/, and each .up.sql
	// file runs exactly once. The schema_migrations table tracks which
	// migrations have been applied so it's safe to run every startup.
	m, err := migrate.New(
		"file://migrations/sim",
		"sqlite3://sims/sim.db?_journal_mode=WAL&_foreign_keys=on",
	)
	if err != nil {
		return fmt.Errorf("creating sim migrator: %w", err)
	}

	if err := m.Up(); err != nil && err != migrate.ErrNoChange {
		return fmt.Errorf("applying sim migrations: %w", err)
	}

	// Close the migrator before opening our own connection — golang-migrate
	// holds a database handle internally, and SQLite only allows one writer.
	m.Close()

	simDB, err := sql.Open("sqlite3", "sims/sim.db?_journal_mode=WAL&_foreign_keys=on")
	if err != nil {
		return fmt.Errorf("opening sim.db: %w", err)
	}
	defer simDB.Close()

	// Insert a new run row. We'll update it with totals at the end.
	agentModel := cfg.Driver.Model
	if agentModel == "" {
		agentModel = fallbackSimAgentModel
	}

	// embedModel captures the model name for the report + sim.db. If no
	// embedding model is configured, we record "(none)" so it's clear in
	// comparison queries that embeddings were disabled for this run.
	embedModel := cfg.Embed.Model
	if embedModel == "" {
		embedModel = "(none)"
	}

	memoryModel := cfg.MemoryAgent.Model
	moodModel := cfg.MoodAgent.Model

	res, err := simDB.Exec(
		`INSERT INTO sim_runs (suite_name, suite_path, chat_model, agent_model, embed_model, memory_model, mood_model, total_messages)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		s.Name, suiteFlag, cfg.Chat.Model, agentModel, embedModel, memoryModel, moodModel, len(s.Messages),
	)
	if err != nil {
		return fmt.Errorf("inserting sim run: %w", err)
	}
	runID, _ := res.LastInsertId()
	conversationID := fmt.Sprintf("sim-run-%d", runID)

	log.Info("Sim run created", "run_id", runID, "conversation_id", conversationID)

	// ------------------------------------------------------------------
	// 4. Create temp DB for the pipeline (clean-room)
	// ------------------------------------------------------------------

	// os.CreateTemp creates a file in the OS temp directory with a unique
	// name. The "*" in the pattern gets replaced with a random string.
	// This is like Python's tempfile.NamedTemporaryFile(delete=False).
	tmpFile, err := os.CreateTemp("", "her-sim-*.db")
	if err != nil {
		return fmt.Errorf("creating temp DB file: %w", err)
	}
	tmpDBPath := tmpFile.Name()
	tmpFile.Close() // Close immediately — NewStore will reopen it.

	// Clean up the temp DB when we're done. defer runs when the function
	// returns — like a Python context manager's __exit__, but for any
	// cleanup action. Multiple defers run in LIFO order (last in, first out).
	defer os.Remove(tmpDBPath)

	store, err := memory.NewStore(tmpDBPath, cfg.Embed.Dimension)
	if err != nil {
		return fmt.Errorf("creating temp store: %w", err)
	}
	defer store.Close()
	store.AutoLinkCount = cfg.Memory.AutoLinkCount
	store.AutoLinkThreshold = cfg.Memory.AutoLinkThreshold

	// ------------------------------------------------------------------
	// 5. Create LLM + embed + search clients (same pattern as run.go)
	// ------------------------------------------------------------------

	chatClient := llm.NewClient(
		cfg.LLM.BaseURL,
		cfg.LLM.APIKey,
		cfg.Chat.Model,
		cfg.Chat.Temperature,
		cfg.Chat.MaxTokens,
	)
	if cfg.Chat.Timeout > 0 {
		chatClient.WithTimeout(time.Duration(cfg.Chat.Timeout) * time.Second)
	}
	if cfg.Chat.Provider != nil {
		chatClient.WithProvider(&llm.ProviderRouting{Order: cfg.Chat.Provider.Order, Only: cfg.Chat.Provider.Only, Sort: cfg.Chat.Provider.Sort})
	}
	if cfg.Chat.Fallback != nil {
		chatClient.WithFallback(cfg.Chat.Fallback.Model, cfg.Chat.Fallback.Temperature, cfg.Chat.Fallback.MaxTokens)
	}
	if disableReasoningFlag {
		disabled := false
		chatClient.WithReasoning(&llm.ReasoningControl{Enabled: &disabled})
	}
	if chatProviderFlag != "" {
		// Split "Groq,Together" → ["Groq", "Together"] for the provider order list.
		// strings.Split on a single value produces a one-element slice, which is fine.
		// --chat-provider overrides the config's provider routing for this run.
		providers := strings.Split(chatProviderFlag, ",")
		chatClient.WithProvider(&llm.ProviderRouting{Order: providers})
		log.Info("chat model provider pinned via --chat-provider", "providers", providers)
	}

	agentTemp := cfg.Driver.Temperature
	if agentTemp == 0 {
		agentTemp = 0.1
	}
	agentMaxTokens := cfg.Driver.MaxTokens
	if agentMaxTokens == 0 {
		agentMaxTokens = 512
	}
	driverClient := llm.NewClient(
		cfg.LLM.BaseURL,
		cfg.LLM.APIKey,
		agentModel,
		agentTemp,
		agentMaxTokens,
	)
	if cfg.Driver.Fallback != nil {
		driverClient.WithFallback(cfg.Driver.Fallback.Model, cfg.Driver.Fallback.Temperature, cfg.Driver.Fallback.MaxTokens)
	}
	if disableReasoningFlag {
		disabled := false
		driverClient.WithReasoning(&llm.ReasoningControl{Enabled: &disabled})
		log.Info("Reasoning disabled for driver model via --disable-reasoning")
	}

	// --- Classifier client (optional) ---
	// Enable the classifier in sims so we can test rejection behavior.
	if classifierModelFlag != "" {
		cfg.Classifier.Model = classifierModelFlag
		log.Info("Classifier model overridden via --classifier-model", "model", classifierModelFlag)
	}
	var classifierClient *llm.Client
	if cfg.Classifier.Model != "" {
		classifierMaxTokens := cfg.Classifier.MaxTokens
		if classifierMaxTokens == 0 {
			classifierMaxTokens = 64
		}
		classifierClient = llm.NewClient(cfg.LLM.BaseURL, cfg.LLM.APIKey, cfg.Classifier.Model, cfg.Classifier.Temperature, classifierMaxTokens)
		log.Info("classifier enabled for sim", "model", cfg.Classifier.Model)
	}

	// --- Vision client (optional) ---
	// Mirrors cmd/run.go: create a vision LLM if the config section is set.
	// This lets sim suites include image messages that flow through the real
	// view_image → Gemini Flash pipeline. Nil when vision isn't configured.
	var visionClient *llm.Client
	if cfg.Vision.Model != "" {
		visionClient = llm.NewClient(cfg.LLM.BaseURL, cfg.LLM.APIKey, cfg.Vision.Model, cfg.Vision.Temperature, cfg.Vision.MaxTokens)
		if cfg.Vision.Fallback != nil {
			visionClient.WithFallback(cfg.Vision.Fallback.Model, cfg.Vision.Fallback.Temperature, cfg.Vision.Fallback.MaxTokens)
		}
		log.Info("vision enabled for sim", "model", cfg.Vision.Model)
	}

	var embedClient *embed.Client
	if cfg.Embed.BaseURL != "" && cfg.Embed.Model != "" {
		embedClient = embed.NewClient(cfg.Embed.BaseURL, cfg.Embed.Model, cfg.Embed.APIKey, cfg.Embed.Dimension)
	}

	var tavilyClient *search.TavilyClient
	if cfg.Search.TavilyAPIKey != "" {
		tavilyClient = search.NewTavilyClient(cfg.Search.TavilyAPIKey, cfg.Search.TavilyBaseURL)
	}

	// --- Memory agent client (needed for run_dream support) ---
	// The dreaming functions use the memory agent LLM
	// because it's the same kind of task: nuanced language about the self.
	var memoryAgentClient *llm.Client
	if cfg.MemoryAgent.Model != "" {
		maTemp := cfg.MemoryAgent.Temperature
		if maTemp == 0 {
			maTemp = 0.3
		}
		maTokens := cfg.MemoryAgent.MaxTokens
		if maTokens == 0 {
			maTokens = 4096
		}
		memoryAgentClient = llm.NewClient(cfg.LLM.BaseURL, cfg.LLM.APIKey, cfg.MemoryAgent.Model, maTemp, maTokens)
		if disableReasoningFlag {
			disabled := false
			memoryAgentClient.WithReasoning(&llm.ReasoningControl{Enabled: &disabled})
		}
	}

	// --- Mood agent client (optional) ---
	// The mood agent runs parallel to the memory agent after each turn.
	// In sim mode we run it in PURE INFERRING mode: ConfidenceHigh is
	// collapsed to ConfidenceLow so every passing inference auto-logs
	// as source=inferred. There's no human to tap proposals during a
	// sim, and dropping mediums would lose data we want to evaluate.
	var moodAgentClient *llm.Client
	var moodRunner *mood.Runner
	if cfg.MoodAgent.Model != "" {
		mTemp := cfg.MoodAgent.Temperature
		if mTemp == 0 {
			mTemp = 0.2
		}
		mTokens := cfg.MoodAgent.MaxTokens
		if mTokens == 0 {
			mTokens = 512
		}
		moodAgentClient = llm.NewClient(cfg.LLM.BaseURL, cfg.LLM.APIKey, cfg.MoodAgent.Model, mTemp, mTokens)

		vocab := mood.Default()
		if cfg.Mood.VocabPath != "" {
			v, err := mood.LoadVocab(cfg.Mood.VocabPath)
			if err != nil {
				return fmt.Errorf("loading mood vocab: %w", err)
			}
			vocab = v
		}

		// Pure-inferring config — one threshold does both jobs.
		low := cfg.Mood.ConfidenceLow
		if low == 0 {
			low = 0.40
		}
		dedupWin := time.Duration(cfg.Mood.DedupWindowMinutes) * time.Minute
		if dedupWin == 0 {
			dedupWin = 2 * time.Hour
		}
		dedupSim := cfg.Mood.DedupSimilarity
		if dedupSim == 0 {
			dedupSim = 0.80
		}
		ctxTurns := cfg.Mood.ContextTurns
		if ctxTurns == 0 {
			ctxTurns = 5
		}

		// In sim the mood classifier and embed client follow the main
		// ones — same model settings, so no extra cost beyond what the
		// real bot would pay.
		embedForMood := func(_ context.Context, text string) ([]float32, error) {
			if embedClient == nil {
				return nil, nil
			}
			return embedClient.Embed(text)
		}

		moodRunner = &mood.Runner{
			Deps: mood.Deps{
				LLM:        moodAgentClient,
				Classifier: classifierClient, // reuse main classifier
				Store:      store,
				Vocab:      vocab,
				Embed:      embedForMood,
				PromptDir:  filepath.Dir(cfg.Persona.PromptFile),
				// Propose deliberately nil — in sim we don't
				// emit proposals. ConfidenceHigh=Low ensures we
				// never hit the emit-proposal path anyway.
			},
			Config: mood.AgentConfig{
				ContextTurns:    ctxTurns,
				ConfidenceHigh:  low, // sim quirk: collapse the two tiers
				ConfidenceLow:   low,
				DedupWindow:     dedupWin,
				DedupSimilarity: dedupSim,
			},
		}
		log.Info("mood agent enabled for sim (pure-inferring mode)",
			"model", cfg.MoodAgent.Model, "threshold", low)
	}

	// ------------------------------------------------------------------
	// 6. Override persona file to a temp empty file
	// ------------------------------------------------------------------

	// The persona file normally accumulates across conversations. For
	// simulations we want a blank slate, so we create an empty temp file.
	tmpPersona, err := os.CreateTemp("", "her-sim-persona-*.md")
	if err != nil {
		return fmt.Errorf("creating temp persona file: %w", err)
	}
	tmpPersonaPath := tmpPersona.Name()
	tmpPersona.Close()
	defer os.Remove(tmpPersonaPath)

	cfg.Persona.PersonaFile = tmpPersonaPath

	// ------------------------------------------------------------------
	// 6.5. Create FakeBridge for calendar operations in sim mode
	// ------------------------------------------------------------------

	// Create an in-memory calendar bridge for sims. This bypasses the
	// Swift EventKit binary requirement and lets us seed calendar state
	// via YAML without permissions or external dependencies.
	var fakeBridge *calendar.FakeBridge
	if len(cfg.Calendar.Calendars) > 0 {
		fakeBridge = calendar.NewFakeBridge(cfg.Calendar.Calendars)
		log.Info("created FakeBridge for calendar operations", "calendars", cfg.Calendar.Calendars)
	}

	// ------------------------------------------------------------------
	// 7. Message loop — send each message through the real pipeline
	// ------------------------------------------------------------------

	// --- Seed memories (if configured) ---
	// Pre-populate the DB with memories before the conversation starts.
	// Each seed goes through the real embedding pipeline so recall_memories
	// can find them via semantic search. Useful for testing recall-dependent
	// features (inbox cleanup, split requests) without needing earlier turns
	// to establish the memories first.
	if len(s.SeedMemories) > 0 {
		log.Infof("  seeding %d memories...", len(s.SeedMemories))
		for _, content := range s.SeedMemories {
			var vec []float32
			if embedClient != nil {
				var err error
				vec, err = embedClient.Embed(content)
				if err != nil {
					log.Warn("seed embed failed, saving without vector", "err", err, "memory", content[:min(len(content), 50)])
				}
			}
			id, err := store.SaveMemory(content, "", "user", 0, 5, vec, vec, "", "")
			if err != nil {
				log.Error("seed memory failed", "err", err)
				continue
			}
			if embedClient != nil && len(vec) > 0 {
				_ = store.AutoLinkMemory(id, vec)
			}
			log.Infof("    seeded #%d: %s", id, content[:min(len(content), 60)])
		}
		log.Info("  seeding complete")
	}

	// --- Seed calendar events (if configured) ---
	// Pre-populate DB (SQLite = source of truth) + FakeBridge (EventKit simulation).
	// Can be any type of event: meetings, shifts, appointments, etc.
	if len(s.SeedCalendarEvents) > 0 {
		log.Infof("  seeding %d calendar events...", len(s.SeedCalendarEvents))
		var fakeEvents []*calendar.FakeEvent
		for _, seed := range s.SeedCalendarEvents {
			// Parse times
			start, err := time.Parse(time.RFC3339, seed.Start)
			if err != nil {
				log.Error("seed calendar event: invalid start", "err", err, "id", seed.ID)
				continue
			}
			end, err := time.Parse(time.RFC3339, seed.End)
			if err != nil {
				log.Error("seed calendar event: invalid end", "err", err, "id", seed.ID)
				continue
			}

			// Default calendar if not specified
			cal := seed.Calendar
			if cal == "" {
				cal = cfg.Calendar.DefaultCalendar
			}

			// Insert into SQLite (source of truth)
			dbID, err := store.InsertCalendarEvent(
				seed.Title,
				seed.Start,
				seed.End,
				seed.Location,
				seed.Notes,
				cal,
				seed.ID, // EventKit identifier
				seed.Job,
			)
			if err != nil {
				log.Error("seed calendar event: DB insert failed", "err", err, "id", seed.ID)
				continue
			}

			// Also seed the FakeBridge so calendar_list returns it
			if fakeBridge != nil {
				fakeEvent := &calendar.FakeEvent{
					ID:       seed.ID,
					Title:    seed.Title,
					Start:    start,
					End:      end,
					Location: seed.Location,
					Notes:    seed.Notes,
					Calendar: cal,
				}
				fakeEvents = append(fakeEvents, fakeEvent)
			}

			// Log with job indicator if this is a shift event
			if seed.Job != "" {
				log.Infof("    seeded #%d: %s [%s shift] on %s (event %s)", dbID, seed.Title, seed.Job, start.Format("Jan 2"), seed.ID)
			} else {
				log.Infof("    seeded #%d: %s on %s (event %s)", dbID, seed.Title, start.Format("Jan 2"), seed.ID)
			}
		}

		if fakeBridge != nil && len(fakeEvents) > 0 {
			fakeBridge.Seed(fakeEvents)
		}
		log.Info("  calendar event seeding complete")
	}

	// turnResults collects the bot's reply for each turn so we can build
	// the report afterward. make() pre-allocates the slice with capacity
	// for all messages — like Python's [None] * n but for a typed slice.
	// Apply --limit flag: if set, only send the first N messages.
	// This lets you test with `her sim --suite sims/intro.yaml -n 1`
	// to just run one message without burning through all your tokens.
	messages := s.Messages
	if limitFlag > 0 && limitFlag < len(messages) {
		messages = messages[:limitFlag]
		log.Infof("limited to first %d messages via --limit", limitFlag)
	}

	turnResults := make([]simTurnResult, 0, len(messages))

	total := len(messages)
	for i, msg := range messages {
		turnStart := time.Now()

		// Build the user-visible text. For image messages this includes
		// the "[User sent a photo]" prefix, mirroring Telegram's behavior
		// in bot/handlers_media.go.
		userText := msg.DisplayText()

		log.Infof("[%d/%d] %s: %s", i+1, total, cfg.Identity.User, userText)

		// If this message includes an image, read it from disk and
		// base64-encode it — same pipeline as bot/handlers_media.go.
		// http.DetectContentType sniffs the MIME from the first 512 bytes.
		var imageBase64, imageMIME string
		if msg.IsImage() {
			imgBytes, err := os.ReadFile(msg.Image)
			if err != nil {
				log.Error("failed to read image file", "err", err, "path", msg.Image)
				turnResults = append(turnResults, simTurnResult{
					userMsg:  userText,
					botReply: fmt.Sprintf("[ERROR: could not read image %s: %s]", msg.Image, err),
					elapsed:  time.Since(turnStart),
				})
				continue
			}
			imageBase64 = base64.StdEncoding.EncodeToString(imgBytes)
			imageMIME = http.DetectContentType(imgBytes)
			log.Infof("  image: %s (%s, %d bytes)", msg.Image, imageMIME, len(imgBytes))

			if visionClient == nil {
				log.Warn("image message but no vision model configured — view_image tool will return an error")
			}
		}

		// Save the user message to the temp store.
		msgID, err := store.SaveMessage("user", userText, "", conversationID)
		if err != nil {
			log.Error("failed to save message", "err", err)
			continue
		}

		// Scrub PII from the message, just like the real pipeline does.
		scrubResult := scrub.Scrub(userText)
		if err := store.UpdateMessageScrubbed(msgID, scrubResult.Text); err != nil {
			log.Error("failed to update scrubbed content", "err", err)
		}

		// StatusCallback updates the user on what the agent is doing.
		// In production this edits the Telegram message; here we just
		// print to stdout so you can watch the agent think.
		statusCallback := func(status string) error {
			log.Infof("  [status] %s", status)
			return nil
		}

		// TraceCallback surfaces agent internals in the sim output.
		// In production this edits a single Telegram message (so sending
		// the full accumulated trace is fine — it just overwrites).
		// In sim mode we only want the NEW line each call, not the full
		// trace re-dumped every time. We track the last text we printed
		// and only output the lines that were appended since then.
		var lastTraceText string
		traceCallback := func(html string) error {
			if html == lastTraceText {
				return nil // nothing new
			}
			// Find the new suffix: everything after what we already printed.
			newPart := html
			if lastTraceText != "" && strings.HasPrefix(html, lastTraceText) {
				newPart = strings.TrimPrefix(html, lastTraceText)
				newPart = strings.TrimLeft(newPart, "\n")
			}
			lastTraceText = html
			for _, line := range strings.Split(strings.TrimSpace(newPart), "\n") {
				if line != "" {
					log.Infof("  [trace] %s", line)
				}
			}
			return nil
		}

		// Turn tracker — nil bus and nil stopTypingFn for sim mode.
		// tracker.Wait() blocks until all phases (main, memory) finish,
		// giving us deterministic ordering for sim assertions.
		tracker := turn.NewTracker(msgID, nil, nil, "", conversationID)

		// Capture any AgentEvent fired by notify_agent so we can run
		// the follow-up synchronously after the memory agent finishes.
		var inboxEvent *agent.AgentEvent
		agentEventCB := func(summary, directMessage string) {
			inboxEvent = &agent.AgentEvent{
				Type:          agent.EventInboxReady,
				Summary:       summary,
				DirectMessage: directMessage,
			}
		}

		// Run the full agent pipeline — same call the Telegram bot makes.
		result, err := agent.Run(agent.RunParams{
			DriverLLM:            driverClient,
			MemoryAgentLLM:       memoryAgentClient, // nil if not configured — memory agent skips
			ChatLLM:             chatClient,
			VisionLLM:           visionClient,      // nil if no vision model configured
			ClassifierLLM:       classifierClient,   // nil if not configured, active if classifier section in config
			ImageBase64:         imageBase64,         // empty if no image this turn
			ImageMIME:           imageMIME,           // empty if no image this turn
			Store:               store,
			EmbedClient:         embedClient,
			SimilarityThreshold: cfg.Embed.SimilarityThreshold,
			TavilyClient:        tavilyClient,
			CalendarBridge:      fakeBridge, // nil in production, FakeBridge in sims
			Cfg:                 cfg,
			ScrubbedUserMessage: scrubResult.Text,
			ScrubVault:          scrubResult.Vault,
			ConversationID:      conversationID,
			TriggerMsgID:        msgID,
			StatusCallback:      statusCallback,
			TraceCallback:       traceCallback,
			TTSCallback:         nil, // no TTS in sim
			ConfigPath:          cfgFile,
			AgentEventCB:        agentEventCB,
			Tracker:             tracker,
		})
		if err != nil {
			log.Error("agent.Run failed", "turn", i+1, "err", err)
			log.Errorf("  %s: [ERROR: %s]", cfg.Identity.Her, err)
			turnResults = append(turnResults, simTurnResult{
				userMsg:  userText,
				botReply: fmt.Sprintf("[ERROR: %s]", err),
				elapsed:  time.Since(turnStart),
			})
			continue
		}

		elapsed := time.Since(turnStart)
		log.Infof("  %s: %s", cfg.Identity.Her, result.ReplyText)
		log.Infof("  (%s)", elapsed.Round(time.Millisecond))

		// Wait for all background agents (main, memory) to finish before
		// checking for inbox events or proceeding to the next turn.
		tracker.Wait()

		// If the memory agent called notify_agent, handle the follow-up
		// synchronously — either a direct message or a brief agent loop.
		var followUpReply string
		if inboxEvent != nil {
			if inboxEvent.DirectMessage != "" {
				followUpReply = inboxEvent.DirectMessage
				log.Infof("  %s (follow-up): %s", cfg.Identity.Her, followUpReply)
			} else {
				// Run a brief agent loop for a natural follow-up message.
				followUpPrompt := fmt.Sprintf(
					"[system] A background task has completed. Summary: %s\n\n"+
						"Briefly update the user on what was done. Keep it to 1-2 sentences — "+
						"this is a follow-up, not a new conversation.",
					inboxEvent.Summary)
				followUpResult, followUpErr := agent.Run(agent.RunParams{
					DriverLLM:            driverClient,
					ChatLLM:             chatClient,
					Store:               store,
					EmbedClient:         embedClient,
					SimilarityThreshold: cfg.Embed.SimilarityThreshold,
					Cfg:                 cfg,
					ScrubbedUserMessage: followUpPrompt,
					ConversationID:      "inbox-followup",
					TriggerMsgID:        msgID,
					StatusCallback:      statusCallback,
					TraceCallback:       traceCallback,
					ConfigPath:          cfgFile,
				})
				if followUpErr == nil {
					followUpReply = followUpResult.ReplyText
					log.Infof("  %s (follow-up): %s", cfg.Identity.Her, followUpReply)
				} else {
					log.Error("follow-up agent.Run failed", "err", followUpErr)
				}
			}
			inboxEvent = nil
		}

		turnResults = append(turnResults, simTurnResult{
			userMsg:       userText,
			botReply:      result.ReplyText,
			followUpReply: followUpReply,
			elapsed:       elapsed,
		})

		// --- Mood agent ---
		// Runs synchronously in sim mode so the report captures its
		// output per turn. Errors are logged, never fatal — the mood
		// agent is best-effort.
		if moodRunner != nil {
			moodRes := moodRunner.RunForConversation(context.Background(), conversationID)
			switch moodRes.Action {
			case mood.ActionAutoLogged:
				fmt.Printf("       [mood] logged: valence=%d labels=%v conf=%.2f\n",
					moodRes.Entry.Valence, moodRes.Entry.Labels, moodRes.Confidence)
			case mood.ActionUpdated:
				fmt.Printf("       [mood] updated #%d: valence=%d labels=%v conf=%.2f\n",
					moodRes.Entry.ID, moodRes.Entry.Valence, moodRes.Entry.Labels, moodRes.Confidence)
			case mood.ActionDroppedLow, mood.ActionDroppedNoSignal, mood.ActionDroppedVocab:
				fmt.Printf("       [mood] dropped: %s (%s)\n", moodRes.Action, moodRes.Reason)
			case mood.ActionDroppedDedup:
				fmt.Printf("       [mood] dedup: %s\n", moodRes.Reason)
			case mood.ActionDroppedClassify:
				fmt.Printf("       [mood] classifier rejected: %s\n", moodRes.Reason)
			case mood.ActionErrored:
				fmt.Printf("       [mood] error: %s\n", moodRes.Reason)
			}
		}

		// --- Forced compaction ---
		// When compact_after is set in the suite YAML, force compaction
		// after that turn by calling MaybeCompact with maxHistoryTokens=1.
		// This makes the 75% threshold effectively 0, guaranteeing that
		// compaction fires. The result: earlier messages become a summary,
		// and the agent can no longer see the original text. This creates
		// the conditions where recall_memories genuinely adds value.
		if s.CompactAfter > 0 && (i+1) == s.CompactAfter {
			log.Infof("  [compact] forced compaction after turn %d (compact_after: %d)", i+1, s.CompactAfter)

			// Load all messages for this conversation — same as the
			// agent does before its compaction check.
			allMsgs, err := store.RecentMessages(conversationID, 1000)
			if err != nil {
				log.Error("forced compaction: failed to load messages", "err", err)
			} else {
				// maxHistoryTokens=1 forces the threshold to 0, so any
				// content triggers compaction. This goes through the real
				// code path — LLM summarization, summary storage, the works.
				cr, err := compact.MaybeCompact(
					chatClient, store, conversationID,
					allMsgs, 1, // maxHistoryTokens=1 → threshold=0 → always compact
					cfg.Identity.Her, cfg.Identity.User,
				)
				if err != nil {
					log.Error("forced compaction failed", "err", err)
				} else if cr.DidCompact {
					log.Infof("  [compact] compacted %d messages into summary (%d tokens before → %d after)",
						cr.Summarized, cr.TokensBefore, cr.TokensAfter)
				} else {
					log.Warn("  [compact] compaction triggered but did not run (not enough unsummarized messages — need at least 7)")
				}
			}
		}

		// Pause between turns to avoid hitting rate limits on free-tier
		// models. In real usage the user types slowly enough that this
		// isn't needed, but sim fires back-to-back.
		if delayFlag > 0 && i < total-1 {
			log.Infof("  (waiting %ds before next turn...)", delayFlag)
			time.Sleep(time.Duration(delayFlag) * time.Second)
		}
	}

	totalDuration := time.Since(startTime)

	// ------------------------------------------------------------------
	// 8. Copy data from temp DB to sim.db
	// ------------------------------------------------------------------

	// We open the temp DB a second time with raw sql.Open to query it
	// directly. The Store struct doesn't expose its internal *sql.DB,
	// and we need to run raw SELECT queries that don't map to any
	// existing Store method. This is fine — SQLite supports concurrent
	// readers via WAL mode.
	tmpDB, err := sql.Open("sqlite3", tmpDBPath+"?_journal_mode=WAL&mode=ro")
	if err != nil {
		return fmt.Errorf("reopening temp DB for copy: %w", err)
	}
	defer tmpDB.Close()

	// Copy messages
	if err := copyMessages(tmpDB, simDB, runID, conversationID); err != nil {
		log.Error("failed to copy messages", "err", err)
	}

	// Copy memories
	if err := copyMemories(tmpDB, simDB, runID); err != nil {
		log.Error("failed to copy memories", "err", err)
	}

	// Copy mood entries
	if err := copyMoodEntries(tmpDB, simDB, runID); err != nil {
		log.Error("failed to copy mood entries", "err", err)
	}

	// Copy classifier verdicts (memory quality, reply safety, style gates)
	if err := copyClassifierLog(tmpDB, simDB, runID); err != nil {
		log.Error("failed to copy classifier log", "err", err)
	}

	// Copy inter-agent communication (send_task, notify_agent inbox)
	if err := copyInboxMessages(tmpDB, simDB, runID); err != nil {
		log.Error("failed to copy inbox messages", "err", err)
	}

	// Copy calendar events
	if err := copyCalendarEvents(tmpDB, simDB, runID); err != nil {
		log.Error("failed to copy calendar events", "err", err)
	}

	// Copy metrics and calculate total cost
	totalCost, err := copyMetrics(tmpDB, simDB, runID)
	if err != nil {
		log.Error("failed to copy metrics", "err", err)
	}

	// Copy agent turns
	if err := copyAgentTurns(tmpDB, simDB, runID, total); err != nil {
		log.Error("failed to copy agent turns", "err", err)
	}

	// Copy compaction summaries — these show when conversation history
	// exceeded the token budget and older messages were compressed into
	// a summary. Without this, compaction is invisible in sim results.
	if err := copySummaries(tmpDB, simDB, runID); err != nil {
		log.Error("failed to copy summaries", "err", err)
	}

	// Update the run row with final totals.
	_, err = simDB.Exec(
		`UPDATE sim_runs SET total_cost_usd = ?, duration_ms = ? WHERE id = ?`,
		totalCost, totalDuration.Milliseconds(), runID,
	)
	if err != nil {
		log.Error("failed to update sim run totals", "err", err)
	}

	// ------------------------------------------------------------------
	// 8b. Optional: run a full dream cycle (run_dream: true in suite YAML)
	// ------------------------------------------------------------------
	// This lets you test the dreaming pipeline without running hundreds of
	// real conversations. The dream uses bypass=true so both gates are skipped —
	// same behaviour as /dream in the Telegram bot.
	var dreamResult simDreamResult
	if s.RunDream && memoryAgentClient != nil {
		dreamResult.Ran = true
		log.Info("[dream] running nightly reflection")
		if err := persona.NightlyReflect(memoryAgentClient, store, cfg, cfg.Identity.Her, cfg.Identity.User); err != nil {
			log.Error("[dream] reflection error", "err", err)
			dreamResult.ReflectError = err.Error()
		} else {
			reflections, _ := store.ReflectionsSince(time.Now().Add(-30 * time.Second))
			if len(reflections) > 0 {
				dreamResult.Reflection = reflections[len(reflections)-1].Content
				log.Infof("[dream] reflection: %s", dreamResult.Reflection)
			} else {
				dreamResult.Reflection = "NOTHING_NOTABLE"
				log.Info("[dream] reflection: NOTHING_NOTABLE")
			}
		}

		minDays := cfg.Persona.MinRewriteDays
		if minDays == 0 {
			minDays = 7
		}
		minRefl := cfg.Persona.MinReflections
		if minRefl == 0 {
			minRefl = 3
		}

		log.Info("[dream] running gated persona rewrite (bypass=true)")
		rewritten, err := persona.GatedRewrite(memoryAgentClient, store, cfg.Persona.PersonaFile, cfg.Identity.Her, true, minDays, minRefl)
		if err != nil {
			log.Error("[dream] rewrite error", "err", err)
			dreamResult.RewriteError = err.Error()
		} else if rewritten {
			dreamResult.Rewritten = true
			data, _ := os.ReadFile(cfg.Persona.PersonaFile)
			dreamResult.PersonaText = string(data)
			log.Infof("[dream] persona rewritten:\n%s", dreamResult.PersonaText)
		} else {
			log.Info("[dream] rewrite: UNCHANGED")
		}
	} else if s.RunDream && memoryAgentClient == nil {
		log.Warn("[dream] skipped — memory_agent.model not configured in config.yaml")
	}

	// ------------------------------------------------------------------
	// 8c. Optional: force the daily mood rollup (run_rollup: true)
	// ------------------------------------------------------------------
	// Mirrors run_dream: the scheduler normally fires the rollup at
	// 21:00 local via cron, but we skip that clock in the sim and
	// invoke the handler directly. Lets us verify aggregation +
	// summary without waiting for a wall-clock day.
	var rollupResult simRollupResult
	if s.RunRollup && moodRunner != nil {
		rollupResult.Ran = true
		fmt.Printf("\n[rollup] Forcing daily mood rollup...\n")

		// Capture the would-be Telegram summary instead of sending
		// anywhere — cmd/sim stays headless.
		var capturedSummary string
		captureSend := func(_ int64, text string) (int, error) {
			capturedSummary = text
			return 0, nil
		}
		deps := &scheduler.Deps{Store: store, Send: captureSend, ChatID: 1}

		// Count dailies before/after so we can tell apart "created a
		// new one" vs "skipped because one already existed".
		before, _ := store.RecentMoodEntries(memory.MoodKindDaily, 1)
		var beforeID int64
		if len(before) > 0 {
			beforeID = before[0].ID
		}

		h := mood.DailyRollupHandler()
		if err := h.Execute(context.Background(), json.RawMessage(`{}`), deps); err != nil {
			rollupResult.Error = err.Error()
			fmt.Printf("[rollup] Error: %v\n", err)
		} else {
			after, _ := store.RecentMoodEntries(memory.MoodKindDaily, 1)
			if len(after) > 0 && after[0].ID != beforeID {
				entry := after[0]
				rollupResult.EntryID = entry.ID
				rollupResult.Valence = entry.Valence
				rollupResult.Labels = entry.Labels
				rollupResult.Associations = entry.Associations
				rollupResult.Note = entry.Note
				rollupResult.SummaryText = capturedSummary
				fmt.Printf("[rollup] Logged daily entry #%d: valence=%d labels=%v\n",
					entry.ID, entry.Valence, entry.Labels)
			} else {
				rollupResult.Skipped = true
				rollupResult.SkipReason = "handler skipped (no momentary entries today or daily already exists)"
				fmt.Printf("[rollup] Skipped — nothing new to log\n")
			}
		}
	} else if s.RunRollup && moodRunner == nil {
		fmt.Printf("\n[rollup] Skipped — mood_agent.model not configured in config.yaml\n")
	}

	// ------------------------------------------------------------------
	// 9. Generate markdown report
	// ------------------------------------------------------------------

	report, err := generateReport(simDB, runID, &s, cfg, turnResults, totalCost, totalDuration, dreamResult, rollupResult)
	if err != nil {
		log.Error("failed to generate report", "err", err)
	} else {
		// Sanitize the suite name for use as a filename. Replace spaces
		// and special characters with hyphens.
		safeName := strings.Map(func(r rune) rune {
			if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' {
				return r
			}
			return '-'
		}, s.Name)
		safeName = strings.ToLower(safeName)

		reportPath := filepath.Join("sims", "results", fmt.Sprintf("%s-run%d.md", safeName, runID))
		if err := os.WriteFile(reportPath, []byte(report), 0o644); err != nil {
			log.Error("failed to write report", "err", err)
		} else {
			log.Infof("report saved: %s", reportPath)
		}
	}

	// ------------------------------------------------------------------
	// 10. Print summary
	// ------------------------------------------------------------------

	log.Info("simulation complete",
		"suite", s.Name,
		"run_id", runID,
		"embed", fmt.Sprintf("%s (dim=%d)", cfg.Embed.Model, cfg.Embed.Dimension),
		"messages", total,
		"duration", totalDuration.Round(time.Millisecond),
		"cost", fmt.Sprintf("$%.4f", totalCost),
		"results", "sims/sim.db",
	)

	return nil
}

// --------------------------------------------------------------------------
// Data copy helpers — move rows from the temp pipeline DB into sim.db
// --------------------------------------------------------------------------

// copyMessages copies all messages from the temp DB into sim_messages,
// tagging each with the run_id. We query turn_number from row ordering
// since messages are inserted sequentially.
func copyMessages(tmpDB, simDB *sql.DB, runID int64, convID string) error {
	rows, err := tmpDB.Query(
		`SELECT id, timestamp, role, content_raw, conversation_id
		 FROM messages WHERE conversation_id = ?
		 ORDER BY id ASC`, convID,
	)
	if err != nil {
		return fmt.Errorf("querying messages: %w", err)
	}
	// defer rows.Close() is critical — without it, the database connection
	// stays locked. Same idea as closing a file handle in Python.
	defer rows.Close()

	turnNum := 0
	for rows.Next() {
		var id int64
		var ts, role, content, cid string
		if err := rows.Scan(&id, &ts, &role, &content, &cid); err != nil {
			return fmt.Errorf("scanning message: %w", err)
		}
		turnNum++
		_, err := simDB.Exec(
			`INSERT INTO sim_messages (run_id, turn_number, timestamp, role, content, conversation_id)
			 VALUES (?, ?, ?, ?, ?, ?)`,
			runID, turnNum, ts, role, content, cid,
		)
		if err != nil {
			return fmt.Errorf("inserting sim_message: %w", err)
		}
	}
	// rows.Err() catches errors that happened during iteration — Next()
	// can silently fail, so this final check is a Go database idiom.
	return rows.Err()
}

// copyMemories copies all memories from the temp DB into sim_memories,
// including supersession tracking (superseded_by, supersede_reason), tags,
// context, and source message ID for full observability.
func copyMemories(tmpDB, simDB *sql.DB, runID int64) error {
	rows, err := tmpDB.Query(
		`SELECT timestamp, memory, category, COALESCE(subject, 'user'), importance, active,
		        superseded_by, supersede_reason, COALESCE(tags, ''), COALESCE(context, ''),
		        source_message_id
		 FROM memories ORDER BY id ASC`,
	)
	if err != nil {
		return fmt.Errorf("querying memories: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var ts, mem, category, subject string
		var tags, context, supersede_reason sql.NullString
		var importance int
		var active bool
		var superseded_by, source_message_id sql.NullInt64
		if err := rows.Scan(&ts, &mem, &category, &subject, &importance, &active,
			&superseded_by, &supersede_reason, &tags, &context, &source_message_id); err != nil {
			return fmt.Errorf("scanning memory: %w", err)
		}
		_, err := simDB.Exec(
			`INSERT INTO sim_memories
			 (run_id, timestamp, memory, category, subject, importance, active,
			  superseded_by, supersede_reason, tags, context, source_message_id)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			runID, ts, mem, category, subject, importance, active,
			superseded_by, supersede_reason, tags, context, source_message_id,
		)
		if err != nil {
			return fmt.Errorf("inserting sim_memory: %w", err)
		}
	}
	return rows.Err()
}

// copySummaries copies compaction summaries from the temp DB into
// sim_summaries. Each row represents one compaction event where older
// messages were compressed into a running summary.
func copySummaries(tmpDB, simDB *sql.DB, runID int64) error {
	rows, err := tmpDB.Query(
		`SELECT timestamp, conversation_id, summary, messages_start_id, messages_end_id
		 FROM summaries ORDER BY id ASC`,
	)
	if err != nil {
		return fmt.Errorf("querying summaries: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var ts, convID, summary string
		var startID, endID int64
		if err := rows.Scan(&ts, &convID, &summary, &startID, &endID); err != nil {
			return fmt.Errorf("scanning summary: %w", err)
		}
		// messages_summarized = how many messages were compressed.
		// endID - startID is approximate but directionally useful.
		msgCount := endID - startID
		if msgCount < 0 {
			msgCount = 0
		}
		_, err := simDB.Exec(
			`INSERT INTO sim_summaries (run_id, timestamp, conversation_id, summary, messages_summarized)
			 VALUES (?, ?, ?, ?, ?)`,
			runID, ts, convID, summary, msgCount,
		)
		if err != nil {
			return fmt.Errorf("inserting sim_summary: %w", err)
		}
	}
	return rows.Err()
}

// copyMoodEntries copies mood entries from the temp DB into
// sim_mood_entries. Schema matches the Apple-style redesign: valence
// 1-7 + labels/associations as JSON + confidence + source.
func copyMoodEntries(tmpDB, simDB *sql.DB, runID int64) error {
	rows, err := tmpDB.Query(
		`SELECT ts, kind, valence, labels, associations, COALESCE(note, ''),
		        source, confidence, COALESCE(conversation_id, '')
		 FROM mood_entries ORDER BY id ASC`,
	)
	if err != nil {
		return fmt.Errorf("querying mood entries: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var (
			ts, kind, labels, associations, note, source, convID string
			valence                                               int
			confidence                                            float64
		)
		if err := rows.Scan(&ts, &kind, &valence, &labels, &associations, &note,
			&source, &confidence, &convID); err != nil {
			return fmt.Errorf("scanning mood entry: %w", err)
		}
		_, err := simDB.Exec(
			`INSERT INTO sim_mood_entries
			   (run_id, ts, kind, valence, labels, associations, note,
			    source, confidence, conversation_id)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			runID, ts, kind, valence, labels, associations, note,
			source, confidence, convID,
		)
		if err != nil {
			return fmt.Errorf("inserting sim_mood_entry: %w", err)
		}
	}
	return rows.Err()
}

// copyClassifierLog copies all classifier verdicts from the temp DB into
// sim_classifier_log. This captures memory quality gates (LOW_VALUE, SPLIT),
// reply safety gates (ESCALATION, DRASTIC_ENDORSEMENT, PURE_VALIDATION),
// reply style gates (STYLE_ISSUE), and soft verdict rewrites. Essential for
// debugging false positives and understanding what gets rejected vs accepted.
func copyClassifierLog(tmpDB, simDB *sql.DB, runID int64) error {
	rows, err := tmpDB.Query(
		`SELECT timestamp, COALESCE(conversation_id, ''), write_type, verdict,
		        content, COALESCE(reason, ''), COALESCE(rewrite, ''), accepted
		 FROM classifier_log ORDER BY id ASC`,
	)
	if err != nil {
		return fmt.Errorf("querying classifier_log: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var ts, convID, writeType, verdict, content, reason, rewrite string
		var accepted sql.NullBool
		if err := rows.Scan(&ts, &convID, &writeType, &verdict, &content, &reason, &rewrite, &accepted); err != nil {
			return fmt.Errorf("scanning classifier_log: %w", err)
		}
		_, err := simDB.Exec(
			`INSERT INTO sim_classifier_log
			   (run_id, timestamp, conversation_id, write_type, verdict, content, reason, rewrite, accepted)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			runID, ts, convID, writeType, verdict, content, reason, rewrite, accepted,
		)
		if err != nil {
			return fmt.Errorf("inserting sim_classifier_log: %w", err)
		}
	}
	return rows.Err()
}

// copyInboxMessages copies inter-agent communication logs from the temp DB's
// inbox table into sim_inbox_messages. This captures send_task calls (cleanup,
// split, update) and notify_agent events — critical for understanding how the
// memory agent responds to lifecycle events and processes background tasks.
func copyInboxMessages(tmpDB, simDB *sql.DB, runID int64) error {
	// The production schema uses the `inbox` table with sender/recipient/msg_type.
	// We extract msg_type as task_type and payload as note for the sim report.
	rows, err := tmpDB.Query(
		`SELECT created_at, msg_type, COALESCE(payload, ''), status
		 FROM inbox ORDER BY id ASC`,
	)
	if err != nil {
		// Inbox table might not exist in older temp DBs — fail gracefully
		if strings.Contains(err.Error(), "no such table") {
			return nil
		}
		return fmt.Errorf("querying inbox: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var ts, msgType, payload, status string
		if err := rows.Scan(&ts, &msgType, &payload, &status); err != nil {
			return fmt.Errorf("scanning inbox message: %w", err)
		}

		// Parse the payload JSON to extract memory_ids if present
		var payloadData struct {
			MemoryIDs []int64 `json:"memory_ids"`
			Note      string  `json:"note"`
		}
		memoryIDs := ""
		note := payload
		if err := json.Unmarshal([]byte(payload), &payloadData); err == nil {
			if len(payloadData.MemoryIDs) > 0 {
				idsBytes, _ := json.Marshal(payloadData.MemoryIDs)
				memoryIDs = string(idsBytes)
			}
			if payloadData.Note != "" {
				note = payloadData.Note
			}
		}

		processed := status == "consumed"

		_, err := simDB.Exec(
			`INSERT INTO sim_inbox_messages
			   (run_id, timestamp, task_type, note, memory_ids, processed, result)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			runID, ts, msgType, note, memoryIDs, processed, "",
		)
		if err != nil {
			return fmt.Errorf("inserting sim_inbox_message: %w", err)
		}
	}
	return rows.Err()
}

// copyCalendarEvents copies all calendar events from the temp DB into
// sim_calendar_events. This captures the final state of the calendar after
// the sim run completes — useful for verifying shift scheduling, event CRUD
// operations, and calendar-related agent behavior.
func copyCalendarEvents(tmpDB, simDB *sql.DB, runID int64) error {
	rows, err := tmpDB.Query(
		`SELECT COALESCE(event_id, ''), title, start, end,
		        COALESCE(location, ''), COALESCE(notes, ''),
		        calendar, COALESCE(job, '')
		 FROM calendar_events
		 WHERE active = 1
		 ORDER BY start ASC`,
	)
	if err != nil {
		return fmt.Errorf("querying calendar events: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var eventID, title, start, end, location, notes, calendar, job string
		if err := rows.Scan(&eventID, &title, &start, &end, &location, &notes, &calendar, &job); err != nil {
			return fmt.Errorf("scanning calendar event: %w", err)
		}
		_, err := simDB.Exec(
			`INSERT INTO sim_calendar_events
			   (run_id, event_id, title, start, end, location, notes, calendar, job)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			runID, eventID, title, start, end, location, notes, calendar, job,
		)
		if err != nil {
			return fmt.Errorf("inserting sim_calendar_event: %w", err)
		}
	}
	return rows.Err()
}

// copyMetrics copies metrics from the temp DB into sim_metrics and returns
// the total cost across all LLM calls in this run.
func copyMetrics(tmpDB, simDB *sql.DB, runID int64) (float64, error) {
	rows, err := tmpDB.Query(
		`SELECT timestamp, model, prompt_tokens, completion_tokens, total_tokens, cost_usd, latency_ms, COALESCE(is_fallback, 0)
		 FROM metrics ORDER BY id ASC`,
	)
	if err != nil {
		return 0, fmt.Errorf("querying metrics: %w", err)
	}
	defer rows.Close()

	var totalCost float64
	for rows.Next() {
		var ts, model string
		var promptTok, completionTok, totalTok, latencyMs int
		var costUSD float64
		var isFallback bool
		if err := rows.Scan(&ts, &model, &promptTok, &completionTok, &totalTok, &costUSD, &latencyMs, &isFallback); err != nil {
			return totalCost, fmt.Errorf("scanning metric: %w", err)
		}
		totalCost += costUSD
		_, err := simDB.Exec(
			`INSERT INTO sim_metrics (run_id, timestamp, model, prompt_tokens, completion_tokens, total_tokens, cost_usd, latency_ms, is_fallback)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			runID, ts, model, promptTok, completionTok, totalTok, costUSD, latencyMs, isFallback,
		)
		if err != nil {
			return totalCost, fmt.Errorf("inserting sim_metric: %w", err)
		}
	}
	return totalCost, rows.Err()
}

// copyAgentTurns copies agent trace data from the temp DB into sim_agent_turns.
// We derive turn_number from message_id ordering — each unique message_id
// represents one conversation turn.
func copyAgentTurns(tmpDB, simDB *sql.DB, runID int64, totalTurns int) error {
	rows, err := tmpDB.Query(
		`SELECT timestamp, message_id, turn_index, role, tool_name, tool_args, content
		 FROM agent_turns ORDER BY id ASC`,
	)
	if err != nil {
		return fmt.Errorf("querying agent turns: %w", err)
	}
	defer rows.Close()

	// Track message_id → turn_number mapping so we can group agent steps
	// by the conversation turn they belong to.
	msgToTurn := make(map[int64]int)
	turnCounter := 0

	for rows.Next() {
		var ts string
		var msgID sql.NullInt64
		var turnIndex int
		var role string
		var toolName, toolArgs, content sql.NullString
		if err := rows.Scan(&ts, &msgID, &turnIndex, &role, &toolName, &toolArgs, &content); err != nil {
			return fmt.Errorf("scanning agent turn: %w", err)
		}

		// Map message_id to a sequential turn number.
		turnNum := 0
		if msgID.Valid {
			if _, exists := msgToTurn[msgID.Int64]; !exists {
				turnCounter++
				msgToTurn[msgID.Int64] = turnCounter
			}
			turnNum = msgToTurn[msgID.Int64]
		}

		_, err := simDB.Exec(
			`INSERT INTO sim_agent_turns (run_id, turn_number, timestamp, turn_index, role, tool_name, tool_args, content)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			runID, turnNum, ts, turnIndex, role, toolName, toolArgs, content,
		)
		if err != nil {
			return fmt.Errorf("inserting sim_agent_turn: %w", err)
		}
	}
	return rows.Err()
}

// --------------------------------------------------------------------------
// Report generation
// --------------------------------------------------------------------------

// generateReport builds a Markdown report summarizing the simulation run.
// It pulls data from both the sim.db (facts, metrics) and the turn results
// collected during the message loop.
func generateReport(
	simDB *sql.DB,
	runID int64,
	s *suite,
	cfg *config.Config,
	turns []simTurnResult,
	totalCost float64,
	totalDuration time.Duration,
	dream simDreamResult,
	rollup simRollupResult,
) (string, error) {
	// strings.Builder is Go's equivalent of Python's io.StringIO or
	// just building a string with a list of parts and joining them.
	// It's more efficient than repeated string concatenation because
	// strings in Go are immutable (each += creates a new string).
	var b strings.Builder

	agentModel := cfg.Driver.Model
	if agentModel == "" {
		agentModel = fallbackSimAgentModel
	}
	reportEmbedModel := cfg.Embed.Model
	if reportEmbedModel == "" {
		reportEmbedModel = "(none)"
	}

	reportMemoryModel := cfg.MemoryAgent.Model
	if reportMemoryModel == "" {
		reportMemoryModel = "(none)"
	}
	reportMoodModel := cfg.MoodAgent.Model
	if reportMoodModel == "" {
		reportMoodModel = "(none)"
	}
	reportClassifierModel := cfg.Classifier.Model
	if reportClassifierModel == "" {
		reportClassifierModel = "(none)"
	}
	reportVisionModel := cfg.Vision.Model
	if reportVisionModel == "" {
		reportVisionModel = "(none)"
	}

	// Header
	fmt.Fprintf(&b, "# Simulation Report: %s\n\n", s.Name)
	fmt.Fprintf(&b, "**Run:** #%d | **Date:** %s | **Cost:** $%.4f\n\n",
		runID,
		time.Now().Format("2006-01-02 15:04"), // Go's time format uses a reference date, not %Y-%m-%d
		totalCost,
	)
	fmt.Fprintf(&b, "| Role | Model |\n|------|-------|\n")
	fmt.Fprintf(&b, "| Chat | %s |\n", cfg.Chat.Model)
	fmt.Fprintf(&b, "| Agent | %s |\n", agentModel)
	fmt.Fprintf(&b, "| Memory | %s |\n", reportMemoryModel)
	fmt.Fprintf(&b, "| Mood | %s |\n", reportMoodModel)
	fmt.Fprintf(&b, "| Classifier | %s |\n", reportClassifierModel)
	fmt.Fprintf(&b, "| Vision | %s |\n", reportVisionModel)
	fmt.Fprintf(&b, "| Embed | %s |\n\n", reportEmbedModel)

	// Conversation section
	b.WriteString("## Conversation\n\n")
	for i, turn := range turns {
		fmt.Fprintf(&b, "### Turn %d\n", i+1)
		fmt.Fprintf(&b, "**%s:** %s\n\n", cfg.Identity.User, turn.userMsg)
		fmt.Fprintf(&b, "**%s:** %s\n\n", cfg.Identity.Her, turn.botReply)

		if turn.followUpReply != "" {
			fmt.Fprintf(&b, "**%s** *(follow-up):* %s\n\n", cfg.Identity.Her, turn.followUpReply)
		}

		// Add agent trace as a collapsible details block.
		writeAgentTrace(&b, simDB, runID, i+1)

		b.WriteString("---\n\n")
	}

	// Memories section (with supersession chains)
	writeMemoriesSection(&b, simDB, runID)

	// Classifier verdicts (memory quality, reply safety, style gates)
	writeClassifierSection(&b, simDB, runID)

	// Inter-agent communication (send_task, notify_agent)
	writeInboxSection(&b, simDB, runID)

	// Mood section
	writeMoodSection(&b, simDB, runID)

	// Calendar events section
	writeCalendarSection(&b, simDB, runID)

	// Compaction summaries section
	writeSummariesSection(&b, simDB, runID)

	// Dream section (only present when run_dream: true)
	writeDreamSection(&b, dream)

	// Forced daily rollup section (only present when run_rollup: true).
	writeRollupSection(&b, rollup)

	// Fallback events (only appears if any calls fell back)
	writeFallbackSection(&b, simDB, runID)

	// Cost summary
	writeCostSection(&b, simDB, runID)

	return b.String(), nil
}

// writeRollupSection writes the forced daily-mood-rollup output to
// the report. Only called when run_rollup: true in the suite YAML.
func writeRollupSection(b *strings.Builder, r simRollupResult) {
	if !r.Ran {
		return
	}
	b.WriteString("## Daily Mood Rollup (forced)\n\n")

	if r.Error != "" {
		fmt.Fprintf(b, "**Error:** %s\n\n", r.Error)
		return
	}
	if r.Skipped {
		fmt.Fprintf(b, "_Skipped — %s._\n\n", r.SkipReason)
		return
	}

	fmt.Fprintf(b, "**Entry #%d logged** — valence %d/7, labels: %s, associations: %s\n\n",
		r.EntryID, r.Valence,
		orDashList(r.Labels), orDashList(r.Associations),
	)
	if r.Note != "" {
		fmt.Fprintf(b, "Note:\n> %s\n\n", r.Note)
	}
	if r.SummaryText != "" {
		b.WriteString("**Summary the bot would have sent:**\n\n")
		b.WriteString("```\n")
		b.WriteString(r.SummaryText)
		b.WriteString("\n```\n\n")
	}
}

// orDashList returns a comma-joined list or "—" when empty. Tiny
// helper for report tables.
func orDashList(items []string) string {
	if len(items) == 0 {
		return "—"
	}
	return strings.Join(items, ", ")
}

// writeDreamSection writes the nightly reflection and persona rewrite output
// to the report. Only called when run_dream: true in the suite YAML.
func writeDreamSection(b *strings.Builder, dream simDreamResult) {
	if !dream.Ran {
		return
	}

	b.WriteString("## Dream Cycle\n\n")

	// Reflection
	b.WriteString("### Nightly Reflection\n\n")
	if dream.ReflectError != "" {
		fmt.Fprintf(b, "**Error:** %s\n\n", dream.ReflectError)
	} else if dream.Reflection == "NOTHING_NOTABLE" {
		b.WriteString("_NOTHING_NOTABLE — reflection found no patterns worth recording._\n\n")
	} else {
		fmt.Fprintf(b, "%s\n\n", dream.Reflection)
	}

	// Persona rewrite
	b.WriteString("### Persona Rewrite\n\n")
	if dream.RewriteError != "" {
		fmt.Fprintf(b, "**Error:** %s\n\n", dream.RewriteError)
	} else if !dream.Rewritten {
		b.WriteString("_UNCHANGED — LLM determined no substantial shift warranted a rewrite._\n\n")
	} else {
		if dream.ChangeSummary != "" {
			fmt.Fprintf(b, "**Change:** %s\n\n", dream.ChangeSummary)
		}
		b.WriteString("**New persona:**\n\n")
		b.WriteString("```\n")
		b.WriteString(dream.PersonaText)
		b.WriteString("\n```\n\n")
	}
}

// writeAgentTrace writes a collapsible <details> block with the agent's
// tool calls for a specific turn number.
func writeAgentTrace(b *strings.Builder, simDB *sql.DB, runID int64, turnNum int) {
	rows, err := simDB.Query(
		`SELECT turn_index, role, tool_name, tool_args, content
		 FROM sim_agent_turns WHERE run_id = ? AND turn_number = ?
		 ORDER BY turn_index ASC`,
		runID, turnNum,
	)
	if err != nil {
		return
	}
	defer rows.Close()

	// Collect all rows first so we know the count for the summary line.
	type agentStep struct {
		turnIndex int
		role      string
		toolName  sql.NullString
		toolArgs  sql.NullString
		content   sql.NullString
	}
	var steps []agentStep

	for rows.Next() {
		var step agentStep
		if err := rows.Scan(&step.turnIndex, &step.role, &step.toolName, &step.toolArgs, &step.content); err != nil {
			continue
		}
		steps = append(steps, step)
	}

	if len(steps) == 0 {
		// No driver trace = the driver model likely failed (rate limit,
		// error, etc.) and the fallback reply kicked in. Flag it loudly.
		b.WriteString("> **⚠ NO DRIVER TRACE** — the driver model produced no tool calls for this turn.\n")
		b.WriteString("> The reply above was generated by the fallback path (direct chat model call).\n")
		b.WriteString("> This usually means the driver model was rate-limited or returned an error.\n\n")
		return
	}

	// Count just the tool calls (assistant role) for the summary line.
	var callCount int
	for _, step := range steps {
		if step.role == "assistant" && step.toolName.Valid {
			callCount++
		}
	}

	// Check if the agent completed its job — a healthy turn always has
	// at least a reply + done. If those are missing, something went wrong.
	var hasReply, hasDone bool
	for _, step := range steps {
		if step.role == "assistant" && step.toolName.Valid {
			switch step.toolName.String {
			case "reply":
				hasReply = true
			case "done":
				hasDone = true
			}
		}
	}

	if !hasReply {
		b.WriteString("> **⚠ INCOMPLETE TURN** — the agent never called `reply`. The response above came from the fallback path.\n\n")
	} else if !hasDone {
		b.WriteString("> **⚠ INCOMPLETE TURN** — the agent called `reply` but never called `done` (loop may have been cut short).\n\n")
	}

	fmt.Fprintf(b, "<details><summary>Agent trace (%d tool calls)</summary>\n\n", callCount)

	// Render each step as a call → result pair. The agent_turns table
	// alternates: assistant (the tool call) then tool (the result).
	// We show both so you can see what the agent decided AND what happened.
	for _, step := range steps {
		if step.role == "assistant" && step.toolName.Valid {
			// This is the agent deciding to call a tool.
			toolName := step.toolName.String
			args := strings.TrimSpace(step.toolArgs.String)
			if args == "" || args == "{}" {
				fmt.Fprintf(b, "**→ `%s`**\n\n", toolName)
			} else {
				// Label malformed/truncated args rather than writing raw whitespace-bloated
				// JSON to the report. This happens when the agent hits max_tokens mid-JSON.
				if !json.Valid([]byte(args)) {
					if len(args) > 300 {
						args = args[:300]
					}
					args = "⚠️ MALFORMED/TRUNCATED: " + args
				}
				fmt.Fprintf(b, "**→ `%s`**\n```json\n%s\n```\n\n", toolName, args)
			}
		} else if step.role == "tool" {
			// This is the tool's response — what actually happened.
			content := step.content.String
			toolName := ""
			if step.toolName.Valid {
				toolName = step.toolName.String
			}
			if content == "" {
				content = "(empty response)"
			}
			// Show the result in a blockquote so it's visually distinct
			// from the call. Indent each line with >.
			lines := strings.Split(content, "\n")
			for _, line := range lines {
				fmt.Fprintf(b, "> %s\n", line)
			}
			if toolName != "" {
				fmt.Fprintf(b, "> *— %s result*\n", toolName)
			}
			b.WriteString("\n")
		}
	}
	b.WriteString("</details>\n\n")
}

// writeMemoriesSection writes the memories table to the report.
func writeMemoriesSection(b *strings.Builder, simDB *sql.DB, runID int64) {
	rows, err := simDB.Query(
		`SELECT id, memory, category, subject, importance, active,
		        superseded_by, supersede_reason
		 FROM sim_memories WHERE run_id = ? ORDER BY id ASC`, runID,
	)
	if err != nil {
		return
	}
	defer rows.Close()

	type memoryRow struct {
		id              int64
		memory          string
		category        sql.NullString
		subject         string
		importance      int
		active          bool
		superseded_by   sql.NullInt64
		supersede_reason sql.NullString
	}
	var memories []memoryRow
	var activeCount, supersededCount int
	for rows.Next() {
		var m memoryRow
		if err := rows.Scan(&m.id, &m.memory, &m.category, &m.subject, &m.importance,
			&m.active, &m.superseded_by, &m.supersede_reason); err != nil {
			continue
		}
		memories = append(memories, m)
		if m.active {
			activeCount++
		} else {
			supersededCount++
		}
	}

	// Header with active/superseded breakdown
	fmt.Fprintf(b, "## Memories (%d total: %d active, %d superseded)\n\n",
		len(memories), activeCount, supersededCount)

	if len(memories) > 0 {
		b.WriteString("| ID | Memory | Category | Subject | Active | Superseded By |\n")
		b.WriteString("|----|--------|----------|---------|--------|---------------|\n")
		for _, m := range memories {
			cat := ""
			if m.category.Valid {
				cat = m.category.String
			}
			activeIcon := "✅"
			if !m.active {
				activeIcon = "❌"
			}
			supersededBy := ""
			if m.superseded_by.Valid {
				supersededBy = fmt.Sprintf("#%d", m.superseded_by.Int64)
			}
			fmt.Fprintf(b, "| %d | %s | %s | %s | %s | %s |\n",
				m.id, m.memory, cat, m.subject, activeIcon, supersededBy)
		}
		b.WriteString("\n")

		// Supersession chains section
		if supersededCount > 0 {
			b.WriteString("### Supersession Chains\n\n")
			b.WriteString("Memories that were updated or corrected:\n\n")
			b.WriteString("| Old ID | Old Memory | → New ID | Reason |\n")
			b.WriteString("|--------|------------|----------|--------|\n")
			for _, m := range memories {
				if !m.active && m.superseded_by.Valid {
					reason := "updated"
					if m.supersede_reason.Valid && m.supersede_reason.String != "" {
						reason = m.supersede_reason.String
					}
					// Truncate old memory to 80 chars for readability
					oldMem := m.memory
					if len(oldMem) > 80 {
						oldMem = oldMem[:77] + "..."
					}
					fmt.Fprintf(b, "| %d | %s | #%d | %s |\n",
						m.id, oldMem, m.superseded_by.Int64, reason)
				}
			}
			b.WriteString("\n")
		}
	}
}

// writeClassifierSection writes the classifier verdicts table to the report.
// Shows all memory quality gates, reply safety gates, and style gates with
// their verdicts, reasons, and soft verdict rewrites. Critical for debugging
// false positives and understanding what gets rejected.
func writeClassifierSection(b *strings.Builder, simDB *sql.DB, runID int64) {
	rows, err := simDB.Query(
		`SELECT write_type, verdict, content, reason, rewrite
		 FROM sim_classifier_log WHERE run_id = ? ORDER BY id ASC`, runID,
	)
	if err != nil {
		return
	}
	defer rows.Close()

	type classifierRow struct {
		write_type string
		verdict    string
		content    string
		reason     string
		rewrite    string
	}
	var verdicts []classifierRow
	var rejectCount, softVerdictCount int
	for rows.Next() {
		var v classifierRow
		if err := rows.Scan(&v.write_type, &v.verdict, &v.content, &v.reason, &v.rewrite); err != nil {
			continue
		}
		verdicts = append(verdicts, v)
		// Count rejections (anything not SAVE/PASS/SAFE)
		if v.verdict != "SAVE" && v.verdict != "PASS" && v.verdict != "SAFE" {
			rejectCount++
		}
		if v.rewrite != "" {
			softVerdictCount++
		}
	}

	if len(verdicts) == 0 {
		b.WriteString("## Classifier Verdicts (0)\n\n_No classifier checks logged._\n\n")
		return
	}

	fmt.Fprintf(b, "## Classifier Verdicts (%d checks: %d rejected, %d soft rewrites)\n\n",
		len(verdicts), rejectCount, softVerdictCount)

	b.WriteString("| Type | Verdict | Content | Reason | Rewrite |\n")
	b.WriteString("|------|---------|---------|--------|----------|\n")
	for _, v := range verdicts {
		// Truncate content to 60 chars for readability
		content := v.content
		if len(content) > 60 {
			content = content[:57] + "..."
		}
		reason := v.reason
		if len(reason) > 50 {
			reason = reason[:47] + "..."
		}
		rewrite := v.rewrite
		if len(rewrite) > 60 {
			rewrite = rewrite[:57] + "..."
		}
		if rewrite == "" {
			rewrite = "—"
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s | %s |\n",
			v.write_type, v.verdict, content, reason, rewrite)
	}
	b.WriteString("\n")
}

// writeInboxSection writes the inter-agent communication log to the report.
// Shows send_task calls (cleanup, split, update) and notify_agent events.
// Critical for understanding how the memory agent processes background tasks.
func writeInboxSection(b *strings.Builder, simDB *sql.DB, runID int64) {
	rows, err := simDB.Query(
		`SELECT task_type, note, memory_ids, processed, result
		 FROM sim_inbox_messages WHERE run_id = ? ORDER BY id ASC`, runID,
	)
	if err != nil {
		return
	}
	defer rows.Close()

	type inboxRow struct {
		task_type  string
		note       string
		memory_ids string
		processed  bool
		result     string
	}
	var tasks []inboxRow
	for rows.Next() {
		var t inboxRow
		if err := rows.Scan(&t.task_type, &t.note, &t.memory_ids, &t.processed, &t.result); err != nil {
			continue
		}
		tasks = append(tasks, t)
	}

	if len(tasks) == 0 {
		b.WriteString("## Inter-Agent Communication (0)\n\n_No inbox messages logged._\n\n")
		return
	}

	fmt.Fprintf(b, "## Inter-Agent Communication (%d messages)\n\n", len(tasks))
	b.WriteString("| Task Type | Note | Memory IDs | Processed | Result |\n")
	b.WriteString("|-----------|------|------------|-----------|--------|\n")
	for _, t := range tasks {
		note := t.note
		if len(note) > 80 {
			note = note[:77] + "..."
		}
		result := t.result
		if len(result) > 60 {
			result = result[:57] + "..."
		}
		if result == "" {
			result = "—"
		}
		processedIcon := "✅"
		if !t.processed {
			processedIcon = "❌"
		}
		fmt.Fprintf(b, "| %s | %s | %s | %s | %s |\n",
			t.task_type, note, t.memory_ids, processedIcon, result)
	}
	b.WriteString("\n")
}

// writeMoodSection writes the mood entries table to the report.
// Columns reflect the Apple-style schema: valence 1-7, JSON-array
// labels and associations, LLM-self-rated confidence, and source.
func writeMoodSection(b *strings.Builder, simDB *sql.DB, runID int64) {
	rows, err := simDB.Query(
		`SELECT ts, kind, valence, labels, associations, note, source, confidence
		 FROM sim_mood_entries WHERE run_id = ? ORDER BY id ASC`, runID,
	)
	if err != nil {
		return
	}
	defer rows.Close()

	type moodRow struct {
		ts, kind, labels, associations, note, source string
		valence                                       int
		confidence                                    float64
	}
	var moods []moodRow
	for rows.Next() {
		var m moodRow
		if err := rows.Scan(&m.ts, &m.kind, &m.valence, &m.labels, &m.associations,
			&m.note, &m.source, &m.confidence); err != nil {
			continue
		}
		moods = append(moods, m)
	}

	fmt.Fprintf(b, "## Mood Entries (%d)\n\n", len(moods))
	if len(moods) == 0 {
		b.WriteString("_No mood inferences this run._\n\n")
		return
	}
	b.WriteString("| Time | Kind | Valence | Labels | Associations | Note | Source | Conf |\n")
	b.WriteString("|------|------|---------|--------|--------------|------|--------|------|\n")
	for _, m := range moods {
		// Turn the raw JSON arrays into comma-separated lists for
		// readability. Fall back to the raw string if decode fails.
		labels := renderJSONArray(m.labels)
		assocs := renderJSONArray(m.associations)
		fmt.Fprintf(b, "| %s | %s | %d | %s | %s | %s | %s | %.2f |\n",
			m.ts, m.kind, m.valence, labels, assocs, m.note, m.source, m.confidence)
	}
	b.WriteString("\n")
}

// writeCalendarSection writes the final calendar state to the report.
// Shows all active calendar events at the end of the sim run, including
// both regular events and shifts (identified by non-empty job field).
func writeCalendarSection(b *strings.Builder, simDB *sql.DB, runID int64) {
	rows, err := simDB.Query(
		`SELECT title, start, end, location, notes, calendar, job
		 FROM sim_calendar_events WHERE run_id = ? ORDER BY start ASC`, runID,
	)
	if err != nil {
		return
	}
	defer rows.Close()

	type calRow struct {
		title, start, end, location, notes, calendar, job string
	}
	var events []calRow
	for rows.Next() {
		var e calRow
		if err := rows.Scan(&e.title, &e.start, &e.end, &e.location, &e.notes, &e.calendar, &e.job); err != nil {
			continue
		}
		events = append(events, e)
	}

	fmt.Fprintf(b, "## Calendar Events (%d)\n\n", len(events))
	if len(events) == 0 {
		b.WriteString("_No calendar events captured this run._\n\n")
		return
	}

	b.WriteString("| Title | Start | End | Calendar | Job | Location | Notes |\n")
	b.WriteString("|-------|-------|-----|----------|-----|----------|-------|\n")
	for _, e := range events {
		// Format timestamps to be more readable (just date + time, drop seconds)
		start := strings.Replace(e.start, "T", " ", 1)
		start = start[:16] // Trim to YYYY-MM-DD HH:MM
		end := strings.Replace(e.end, "T", " ", 1)
		end = end[:16]

		job := e.job
		if job == "" {
			job = "—"
		}
		location := e.location
		if location == "" {
			location = "—"
		}
		notes := strings.ReplaceAll(e.notes, "\n", " / ") // Collapse multiline notes
		if notes == "" {
			notes = "—"
		}

		fmt.Fprintf(b, "| %s | %s | %s | %s | %s | %s | %s |\n",
			e.title, start, end, e.calendar, job, location, notes)
	}
	b.WriteString("\n")
}

// renderJSONArray decodes a JSON string-array into a comma-separated
// display string. Empty input and decode failures render as an em-dash.
func renderJSONArray(s string) string {
	s = strings.TrimSpace(s)
	if s == "" || s == "[]" || s == "null" {
		return "—"
	}
	var items []string
	if err := json.Unmarshal([]byte(s), &items); err != nil || len(items) == 0 {
		return "—"
	}
	return strings.Join(items, ", ")
}

// writeSummariesSection writes any compaction summaries to the report.
// Each summary represents a point where older conversation history was
// compressed to stay within the token budget.
func writeSummariesSection(b *strings.Builder, simDB *sql.DB, runID int64) {
	rows, err := simDB.Query(
		`SELECT timestamp, summary, messages_summarized
		 FROM sim_summaries WHERE run_id = ? ORDER BY id ASC`, runID,
	)
	if err != nil {
		return
	}
	defer rows.Close()

	type summaryRow struct {
		ts               string
		summary          string
		msgsSummarized   int
	}
	var summaries []summaryRow
	for rows.Next() {
		var s summaryRow
		if err := rows.Scan(&s.ts, &s.summary, &s.msgsSummarized); err != nil {
			continue
		}
		summaries = append(summaries, s)
	}

	fmt.Fprintf(b, "## Compaction Events (%d)\n\n", len(summaries))
	if len(summaries) == 0 {
		b.WriteString("_No compaction triggered during this run._\n\n")
	} else {
		for i, s := range summaries {
			fmt.Fprintf(b, "### Compaction %d (%s) — %d messages summarized\n\n", i+1, s.ts, s.msgsSummarized)
			b.WriteString("```\n")
			b.WriteString(s.summary)
			b.WriteString("\n```\n\n")
		}
	}
}

// writeFallbackSection writes the fallback events summary — how many calls
// fell back, which models were involved, and the cost impact. This is the
// "Haiku tax" detector: when free-tier models hit rate limits, the system
// silently falls back to a paid model. Without this section, you'd never
// know your "free" run cost $0.13 in surprise Haiku calls.
func writeFallbackSection(b *strings.Builder, simDB *sql.DB, runID int64) {
	// Get total calls and fallback calls for the percentage.
	var totalCalls, fallbackCalls int
	var fallbackCost float64
	simDB.QueryRow(
		`SELECT COUNT(*), COALESCE(SUM(CASE WHEN is_fallback = 1 THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(CASE WHEN is_fallback = 1 THEN cost_usd ELSE 0 END), 0)
		 FROM sim_metrics WHERE run_id = ?`, runID,
	).Scan(&totalCalls, &fallbackCalls, &fallbackCost)

	if fallbackCalls == 0 {
		return // no fallback events — skip the section entirely
	}

	b.WriteString("## Fallback Events\n\n")
	fmt.Fprintf(b, "**%d of %d calls (%.0f%%) used the fallback model** — $%.4f in fallback costs\n\n",
		fallbackCalls, totalCalls,
		float64(fallbackCalls)/float64(totalCalls)*100,
		fallbackCost,
	)

	// Breakdown by model: show which models were used as fallbacks.
	rows, err := simDB.Query(
		`SELECT model, COUNT(*) as calls, SUM(cost_usd) as cost
		 FROM sim_metrics WHERE run_id = ? AND is_fallback = 1
		 GROUP BY model ORDER BY cost DESC`, runID,
	)
	if err != nil {
		return
	}
	defer rows.Close()

	b.WriteString("| Fallback Model | Calls | Cost |\n")
	b.WriteString("|----------------|-------|------|\n")
	for rows.Next() {
		var model string
		var calls int
		var cost float64
		if rows.Scan(&model, &calls, &cost) == nil {
			fmt.Fprintf(b, "| %s | %d | $%.4f |\n", model, calls, cost)
		}
	}
	b.WriteString("\n")
}

// writeCostSection writes the cost summary table grouped by model,
// with primary vs fallback breakdown.
func writeCostSection(b *strings.Builder, simDB *sql.DB, runID int64) {
	rows, err := simDB.Query(
		`SELECT model,
		        COUNT(*) as calls,
		        SUM(prompt_tokens) as prompt,
		        SUM(completion_tokens) as completion,
		        SUM(total_tokens) as total,
		        SUM(cost_usd) as cost,
		        SUM(CASE WHEN is_fallback = 1 THEN 1 ELSE 0 END) as fallback_calls
		 FROM sim_metrics WHERE run_id = ?
		 GROUP BY model ORDER BY cost DESC`, runID,
	)
	if err != nil {
		return
	}
	defer rows.Close()

	type costRow struct {
		model         string
		calls         int
		prompt        int
		completion    int
		total         int
		cost          float64
		fallbackCalls int
	}
	var costs []costRow
	for rows.Next() {
		var c costRow
		if err := rows.Scan(&c.model, &c.calls, &c.prompt, &c.completion, &c.total, &c.cost, &c.fallbackCalls); err != nil {
			continue
		}
		costs = append(costs, c)
	}

	b.WriteString("## Cost Summary\n\n")
	if len(costs) > 0 {
		b.WriteString("| Model | Calls | Fallback | Prompt | Completion | Total | Cost |\n")
		b.WriteString("|-------|-------|----------|--------|------------|-------|------|\n")
		for _, c := range costs {
			fallbackLabel := "-"
			if c.fallbackCalls > 0 {
				fallbackLabel = fmt.Sprintf("%d", c.fallbackCalls)
			}
			fmt.Fprintf(b, "| %s | %d | %s | %d | %d | %d | $%.4f |\n",
				c.model, c.calls, fallbackLabel, c.prompt, c.completion, c.total, c.cost)
		}
	}
	b.WriteString("\n")
}

// truncate shortens a string to maxLen characters, appending "..." if it
// was truncated. Useful for keeping report output readable.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	// In Go, slicing a string by byte index is fine for ASCII. For full
	// Unicode safety you'd convert to []rune first, but for tool args
	// and debug output this is good enough.
	return s[:maxLen] + "..."
}
