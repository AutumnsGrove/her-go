// Package config loads application configuration from a YAML file with
// environment variable expansion. If a value looks like "${ENV_VAR}",
// it gets replaced with the actual environment variable value at load time.
//
// Defaults are derived from config.yaml.example — that file is the single
// source of truth for default values. The user's config.yaml is layered
// on top, overriding only the fields they specify.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Config is the top-level configuration struct. In Go, structs are like Python
// classes but without methods attached by default — they're just data containers.
// The `yaml:"..."` tags tell the YAML parser which key maps to which field.
// This is called a "struct tag" — metadata attached to fields that libraries
// can read at runtime (similar to Python decorators on steroids).
type Config struct {
	Debug         bool                `yaml:"debug"` // when true, logs full API request/response bodies
	Identity      IdentityConfig      `yaml:"identity"`
	Telegram      TelegramConfig      `yaml:"telegram"`
	LLM           LLMConfig           `yaml:"llm"`
	Chat          ChatConfig          `yaml:"chat"`
	Driver        DriverConfig        `yaml:"driver"`
	Vision        VisionConfig        `yaml:"vision"`
	Classifier    ClassifierConfig    `yaml:"classifier"`
	MemoryAgent   MemoryAgentConfig   `yaml:"memory_agent"`
	MoodAgent     MoodAgentConfig     `yaml:"mood_agent"`
	Memory        MemoryConfig        `yaml:"memory"`
	Mood          MoodConfig          `yaml:"mood"`
	Embed      EmbedConfig      `yaml:"embed"`
	Search     SearchConfig     `yaml:"search"`
	Foursquare FoursquareConfig `yaml:"foursquare"`
	Scrub      ScrubConfig      `yaml:"scrub"`
	Persona    PersonaConfig    `yaml:"persona"`
	Voice      VoiceConfig      `yaml:"voice"`
	Location   LocationConfig   `yaml:"location,omitempty"`
	Calendar   CalendarConfig   `yaml:"calendar"`
	Tunnel     TunnelConfig     `yaml:"tunnel"`
}

// LocationConfig holds the user's saved home coordinates and unit
// preferences, used by the get_weather tool. Lat/lon are written by
// the set_location tool whenever the user updates their home location
// (via SetLocation — a surgical YAML edit that preserves formatting).
//
// Zero values are the "no location set" state; the get_weather tool
// prompts the user to run set_location when it sees 0/0.
type LocationConfig struct {
	Latitude  float64 `yaml:"latitude"`
	Longitude float64 `yaml:"longitude"`
	Name      string  `yaml:"name,omitempty"`       // display name from the last geocoding ("Portland, Oregon")
	TempUnit  string  `yaml:"temp_unit,omitempty"`  // "fahrenheit" (default) or "celsius"
	WindUnit  string  `yaml:"wind_unit,omitempty"`  // "mph" (default) or "kmh"
}

// TunnelConfig holds settings for Cloudflare Tunnel integration. The tunnel
// creates a stable public URL (e.g., her.yourdomain.com) that routes to the
// bot's local webhook server, even behind NAT. Used for always-on deployment
// on the Mac Mini and ephemeral dev sessions on the MacBook.
//
// The tunnel name and credentials come from `cloudflared tunnel create`.
// Domain is the hostname you've routed to this tunnel via DNS.
type TunnelConfig struct {
	Name            string `yaml:"name"`             // tunnel name from `cloudflared tunnel create` (e.g., "her-prod")
	Domain          string `yaml:"domain"`           // public hostname (e.g., "her.yourdomain.com")
	CredentialsFile string `yaml:"credentials_file"` // path to tunnel credentials JSON (e.g., ~/.cloudflared/<tunnel-id>.json)
}

// CalendarConfig holds settings for the Swift EventKit bridge and calendar tools.
// The bridge is optional — if missing at startup, calendar tools return clear
// errors to the agent but don't block bot startup (fail-soft pattern).
type CalendarConfig struct {
	BridgePath      string      `yaml:"bridge_path"`       // path to her-calendar Swift binary
	Calendars       []string    `yaml:"calendars"`         // which calendars to monitor (reads from all)
	DefaultCalendar string      `yaml:"default_calendar"`  // default calendar for creating events
	DefaultTimezone string      `yaml:"default_timezone"`  // e.g. "America/New_York", used by get_time tool
	Jobs            []JobConfig `yaml:"jobs"`              // known jobs for shift tracking (optional)
}

// JobConfig defines a known job for shift tracking. When the agent passes
// a "job" param to calendar_create, MatchJob validates the name and auto-fills
// defaults like location and role. Add or remove jobs freely in config.yaml —
// code never references these by name.
//
// This is like a Python dataclass with default values — the struct holds the
// data and the config methods use it for lookups.
type JobConfig struct {
	Name        string   `yaml:"name"`         // display name (e.g., "Panera")
	Address     string   `yaml:"address"`      // work address — auto-fills event location
	DefaultRole string   `yaml:"default_role"` // default position/role (blank = read from schedule)
	Aliases     []string `yaml:"aliases"`      // alternative names (e.g., ["panera bread"])
}

// MatchJob returns the job whose name or alias matches the given string
// (case-insensitive), or nil if no match. Used by calendar_create to
// validate and auto-fill shift defaults from config.
//
// strings.EqualFold is Go's Unicode-aware case-insensitive compare —
// like Python's .lower() == .lower() but handles edge cases better.
func (c *CalendarConfig) MatchJob(name string) *JobConfig {
	for i := range c.Jobs {
		if strings.EqualFold(c.Jobs[i].Name, name) {
			return &c.Jobs[i]
		}
		for _, alias := range c.Jobs[i].Aliases {
			if strings.EqualFold(alias, name) {
				return &c.Jobs[i]
			}
		}
	}
	return nil
}

// IdentityConfig holds the bot and owner names. These get injected into
// prompt files via {{her}} and {{user}} placeholders, and used as role
// labels in conversation transcripts, tool descriptions, etc.
type IdentityConfig struct {
	Her  string `yaml:"her"`  // the bot's name (default: "Mira")
	User string `yaml:"user"` // the owner's name (default: "Autumn")
}

// ExpandPrompt replaces {{her}} and {{user}} placeholders in a prompt
// string with the configured identity names. This is intentionally simple
// — just two string replacements, no template engine needed.
func (c *Config) ExpandPrompt(content string) string {
	content = strings.ReplaceAll(content, "{{her}}", c.Identity.Her)
	content = strings.ReplaceAll(content, "{{user}}", c.Identity.User)
	return content
}

// TelegramConfig holds Telegram bot settings.
//
// Mode controls the update transport:
//   - "poll" (default): the bot long-polls Telegram every 10 seconds.
//     Simple, works anywhere, no public URL needed.
//   - "webhook": Telegram POSTs updates to us. Requires a public URL
//     (CF Worker + Cloudflare Tunnel) and a listening HTTP server.
//
// In webhook mode, telebot opens an HTTP server on WebhookPort and
// validates the X-Telegram-Bot-Api-Secret-Token header against
// WebhookSecret. The CF Worker forwards updates here — the bot never
// registers itself as the webhook endpoint (IgnoreSetWebhook is set).
type TelegramConfig struct {
	Token          string `yaml:"token"`
	Mode           string `yaml:"mode"`            // "poll" or "webhook"
	WebhookURL     string `yaml:"webhook_url"`     // public URL registered with Telegram (set by CF Worker, not the bot)
	WebhookPort    int    `yaml:"webhook_port"`    // local port for webhook HTTP server (default 8765)
	WebhookSecret  string `yaml:"webhook_secret"`  // shared secret — validated via X-Telegram-Bot-Api-Secret-Token header
	OwnerChat      int64  `yaml:"owner_chat"`      // chat ID for the bot owner — used by scheduler for proactive messages
}

// FallbackConfig holds settings for a fallback model. When the primary
// model fails with a retriable error (rate limit, timeout, server error),
// the system automatically retries with these settings. Uses a pointer
// in parent configs so it's nil when not configured — Go's zero value
// for a pointer is nil, which is perfect for "optional" fields.
type FallbackConfig struct {
	Model       string  `yaml:"model"`
	Temperature float64 `yaml:"temperature"`
	MaxTokens   int     `yaml:"max_tokens"`
}

// ReasoningConfig controls reasoning behavior for models that support
// both reasoning and non-reasoning modes (hybrid models like Qwen3.6, DeepSeek V3.2).
// This maps to OpenRouter's `reasoning` parameter. Pure reasoning models
// (DeepSeek R1, V4) ignore this — they always reason. Pure instruct models
// (Qwen3 235B) don't need it — they never reason.
type ReasoningConfig struct {
	Enabled *bool `yaml:"enabled,omitempty"` // nil = API default, false = disable, true = enable
}

// LLMConfig holds shared OpenRouter / OpenAI-compatible API credentials.
// Model settings live in the per-model sections (chat:, agent:, etc.)
// so each model can be tuned independently without touching the API config.
type LLMConfig struct {
	BaseURL string `yaml:"base_url"`
	APIKey  string `yaml:"api_key"`
}

// ChatConfig holds settings for the chat (reply) model — the model that
// generates the actual user-facing response. Separate from LLMConfig so
// credentials and model tuning are not tangled together.
// Shares the same base_url and api_key as the main LLM section.
type ChatConfig struct {
	Model        string           `yaml:"model"`
	Temperature  float64          `yaml:"temperature"`
	MaxTokens    int              `yaml:"max_tokens"`
	MaxReplyChars int             `yaml:"max_reply_chars"` // reject replies over this length as likely degenerate (0 = 10000 default)
	Timeout      int              `yaml:"timeout"`            // HTTP timeout in seconds (0 = 60s default). A Groq-hosted tool-calling model should respond in <5s — 20s is a reasonable ceiling.
	Provider     *ProviderConfig  `yaml:"provider,omitempty"` // OpenRouter provider routing (optional)
	Fallback     *FallbackConfig  `yaml:"fallback,omitempty"`
	Reasoning    *ReasoningConfig `yaml:"reasoning,omitempty"` // reasoning control for hybrid models (optional)
	Streaming    bool             `yaml:"streaming"`           // stream reply tokens to Telegram for a live typing effect (default false)
}

// DriverConfig holds settings for the driver agent — the orchestrator that
// receives user messages and decides which tools to call (think, recall, search,
// reply, done). This is the primary model in the conversation loop.
type DriverConfig struct {
	Model       string           `yaml:"model"`
	Temperature float64          `yaml:"temperature"`
	MaxTokens   int              `yaml:"max_tokens"`
	Timeout     int              `yaml:"timeout"`             // HTTP timeout in seconds (0 = 60s default)
	Trace       bool             `yaml:"trace"`               // show agent thinking traces in chat
	Fallback    *FallbackConfig  `yaml:"fallback"`            // optional fallback model for when primary is unavailable
	Reasoning   *ReasoningConfig `yaml:"reasoning,omitempty"` // reasoning control for hybrid models (optional)

	// Loop tuning — how many iterations per window and how many continuation
	// windows before giving up. Defaults: 15 iterations, 3 continuations (= 60 max).
	IterationsPerWindow int `yaml:"iterations_per_window"` // 0 = 15
	MaxContinuations    int `yaml:"max_continuations"`     // 0 = 3

	// MaxRepliesPerTurn caps how many reply tool calls the agent can make
	// in a single user turn. Prevents the agent from self-correcting in a
	// loop (think→reply→think→reply with near-identical content). The reply
	// handler's internal style/safety gates already retry once — this cap
	// is a hard ceiling on top of that. Default 2 (enough for "let me look
	// that up" → "here's what I found").
	MaxRepliesPerTurn int `yaml:"max_replies_per_turn"` // 0 = 2
}

// VisionConfig holds settings for the vision language model (VLM).
// Used for image understanding — when the user sends a photo, the
// view_image agent tool calls this model to describe what's in it.
// Shares the same base_url and api_key as the main LLM section.
type VisionConfig struct {
	Model       string          `yaml:"model"`
	Temperature float64         `yaml:"temperature"`
	MaxTokens   int             `yaml:"max_tokens"`
	Fallback    *FallbackConfig `yaml:"fallback"` // optional fallback model for when primary is unavailable
}

// ClassifierConfig holds settings for the classifier LLM gate.
// A small, fast model (Haiku-class) that validates memory writes before
// they hit the DB — catches fictional content (game events, book plots)
// that the agent model mistakes for real user facts.
// Shares the same base_url and api_key as the main LLM section.
type ClassifierConfig struct {
	Model       string  `yaml:"model"`       // e.g. "anthropic/claude-haiku-4.5"
	Temperature float64 `yaml:"temperature"` // 0 for deterministic verdicts
	MaxTokens   int     `yaml:"max_tokens"`  // ~64 is enough for REAL/FICTIONAL
}

// MemoryAgentConfig holds settings for the post-turn background memory agent.
// This model runs after the driver agent delivers its reply — it reviews the
// conversation turn and extracts facts to save. Runs in a goroutine so it
// never blocks the user. A strong narrative-language model is recommended for nuanced fact extraction.
type MemoryAgentConfig struct {
	Model       string           `yaml:"model"`
	Temperature float64          `yaml:"temperature"`
	MaxTokens   int              `yaml:"max_tokens"`
	Timeout     int              `yaml:"timeout"`             // HTTP timeout in seconds (0 = 60s default). Memory agent processes long transcripts — 120s recommended.
	Provider    *ProviderConfig  `yaml:"provider,omitempty"`  // OpenRouter provider routing (optional)
	Fallback    *FallbackConfig  `yaml:"fallback,omitempty"`
	Reasoning   *ReasoningConfig `yaml:"reasoning,omitempty"` // reasoning control for hybrid models (optional)

	// Loop tuning — same as DriverConfig. Defaults: 15 iterations, 2 continuations (= 45 max).
	IterationsPerWindow int `yaml:"iterations_per_window"` // 0 = 15
	MaxContinuations    int `yaml:"max_continuations"`     // 0 = 2
}

// MoodAgentConfig controls the post-turn background mood agent.
// Same shape as MemoryAgentConfig — a strong narrative-language model runs in a
// goroutine after each reply, scoring a structured mood inference
// against the Apple-style vocab. Nil/empty model disables the
// agent at startup.
type MoodAgentConfig struct {
	Model       string          `yaml:"model"`
	Temperature float64         `yaml:"temperature"`
	MaxTokens   int             `yaml:"max_tokens"`
	Timeout     int             `yaml:"timeout"`  // HTTP timeout in seconds (0 = 60s default)
	Provider    *ProviderConfig `yaml:"provider,omitempty"`
	Fallback    *FallbackConfig `yaml:"fallback,omitempty"`
}

// MoodConfig holds behavior knobs for the mood agent + sweeper.
// Defaults match docs/plans/PLAN-mood-tracking-redesign.md.
type MoodConfig struct {
	// VocabPath is the YAML file listing valence buckets, labels,
	// and associations. Empty → use the embedded default.
	VocabPath string `yaml:"vocab_path"`

	// ContextTurns is how many recent user+assistant turns the
	// agent sees. Default 5.
	ContextTurns int `yaml:"context_turns"`

	// ConfidenceHigh — ≥ this → auto-log. Default 0.75.
	ConfidenceHigh float64 `yaml:"confidence_high"`

	// ConfidenceLow — < this → drop silently. Default 0.40.
	ConfidenceLow float64 `yaml:"confidence_low"`

	// DedupWindowMinutes is the KNN dedup lookback. Default 120.
	DedupWindowMinutes int `yaml:"dedup_window_minutes"`

	// DedupSimilarity — cosine similarity threshold for "same
	// mood again". Default 0.80.
	DedupSimilarity float64 `yaml:"dedup_similarity"`

	// ProposalExpiryMinutes is how long a Telegram proposal stays
	// tappable. Default 30.
	ProposalExpiryMinutes int `yaml:"proposal_expiry_minutes"`

	// SweeperIntervalMinutes is how often the expiry sweeper runs.
	// Default 5.
	SweeperIntervalMinutes int `yaml:"sweeper_interval_minutes"`

	// DailyRollupCron is the cron expression for the daily rollup
	// task. Default "0 21 * * *" (9pm local).
	DailyRollupCron string `yaml:"daily_rollup_cron"`
}

// ProviderConfig controls OpenRouter provider routing from config.yaml.
// Maps to the "provider" field in the API request body.
type ProviderConfig struct {
	Order []string `yaml:"order,omitempty"` // try these providers first, in order
	Only  []string `yaml:"only,omitempty"`  // restrict to ONLY these providers
	Sort  string   `yaml:"sort,omitempty"`  // "latency", "throughput", or "price"
}

// MemoryConfig controls the SQLite-backed memory system.
type MemoryConfig struct {
	DBPath             string  `yaml:"db_path"`
	RecentMessages     int     `yaml:"recent_messages"`
	MaxFactsInContext  int     `yaml:"max_facts_in_context"`
	ExtractionInterval int     `yaml:"extraction_interval"`
	MaxHistoryTokens    int     `yaml:"max_history_tokens"`    // history token budget for compaction — both triggers fire at 75% of this
	DriverContextBudget int     `yaml:"driver_context_budget"` // driver model total prompt budget for action history compaction; 0 = use 6000 default
	AutoLinkCount      int     `yaml:"auto_link_count"`       // max links per new fact (0 = disabled)
	AutoLinkThreshold  float64 `yaml:"auto_link_threshold"`   // min cosine similarity to create a link (0.0-1.0)
	MaxMemoryLength    int     `yaml:"max_memory_length"`     // hard character limit for a single memory (0 = use default 300)
}

// ScrubConfig controls PII scrubbing behavior.
type ScrubConfig struct {
	Enabled bool `yaml:"enabled"`
}

// EmbedConfig controls the local embedding model used for semantic
// similarity (memory deduplication and vector search via sqlite-vec).
type EmbedConfig struct {
	BaseURL             string  `yaml:"base_url"`
	Model               string  `yaml:"model"`
	APIKey              string  `yaml:"api_key"`               // optional — needed for remote APIs (OpenRouter, OpenAI), empty for local
	Dimension           int     `yaml:"dimension"`             // vector dimension (768 for nomic-embed-text-v1.5, 1536 for OpenAI, etc.)
	SimilarityThreshold float64 `yaml:"similarity_threshold"`  // above this = duplicate (0.0-1.0)
	MaxSemanticDistance float64 `yaml:"max_semantic_distance"` // facts farther than this are filtered from context (cosine distance, 0=identical)
	StartCommand        string  `yaml:"start_command"`         // optional: shell command to launch the embed server if it's not already running
}

// SearchConfig controls web search and book search integrations.
type SearchConfig struct {
	TavilyAPIKey  string `yaml:"tavily_api_key"`
	TavilyBaseURL string `yaml:"tavily_base_url"` // defaults to https://api.tavily.com
}

// FoursquareConfig holds credentials for the Foursquare Places API v3.
// Used by the nearby_search tool for structured place search (distance,
// categories, open/closed status). Free tier: 10,000 calls/month.
// Empty api_key disables the integration — nearby_search falls back
// to Tavily web search.
type FoursquareConfig struct {
	APIKey string `yaml:"api_key"`
}

// PersonaConfig controls the persona evolution system.
type PersonaConfig struct {
	PromptFile                string  `yaml:"prompt_file"`
	PersonaFile               string  `yaml:"persona_file"`
	AgentPromptFile           string  `yaml:"agent_prompt_file"`
	RewriteEveryNReflections  int     `yaml:"rewrite_every_n_reflections"`
	ReflectionMemoryThreshold int     `yaml:"reflection_memory_threshold"`
	MaxTraitShift             float64 `yaml:"max_trait_shift"`

	// Dreaming system — nightly persona evolution.
	// DreamHour is the local hour (0-23) to run the nightly reflection.
	// 0 uses the default of 4 (04:00).
	DreamHour int `yaml:"dream_hour"`
	// MinRewriteDays is the minimum number of days between persona rewrites.
	// 0 uses the default of 7.
	MinRewriteDays int `yaml:"min_rewrite_days"`
	// MinReflections is the minimum number of unconsumed reflections required
	// before a rewrite is allowed. 0 uses the default of 3.
	MinReflections int `yaml:"min_reflections"`
}

// VoiceConfig controls voice memo processing (STT in v0.3, TTS in v0.5).
type VoiceConfig struct {
	Enabled bool      `yaml:"enabled"`
	STT     STTConfig `yaml:"stt"`
	TTS     TTSConfig `yaml:"tts"`
}

// STTConfig controls speech-to-text. The "parakeet" engine expects a local
// HTTP server (parakeet-mlx-fastapi) running on BaseURL. The Go side just
// POSTs audio files to it — no Python bindings needed.
type STTConfig struct {
	Engine  string `yaml:"engine"`   // "parakeet" or "cf-workers-ai"
	BaseURL string `yaml:"base_url"` // e.g. "http://localhost:8765"
	Model   string `yaml:"model"`    // HuggingFace model ID or local path
}

// TTSConfig controls text-to-speech. The "piper" engine expects a local
// HTTP server (piper TTS sidecar) running on BaseURL with an OpenAI-compatible
// /v1/audio/speech endpoint. The Go side POSTs JSON and gets back WAV bytes.
type TTSConfig struct {
	Enabled   bool    `yaml:"enabled"`
	Engine    string  `yaml:"engine"`     // "piper" (local) — future engines can be added
	BaseURL   string  `yaml:"base_url"`   // e.g. "http://localhost:8766"
	Model     string  `yaml:"model"`      // HuggingFace model ID or local path
	VoiceID   string  `yaml:"voice_id"`   // voice preset (for piper: same as model)
	Speed     float64 `yaml:"speed"`      // speaking rate (1.0 = normal)
	ReplyMode string  `yaml:"reply_mode"` // "voice" (always reply with voice) or "match" (mirror input format)
}

// envVarPattern matches "${VARIABLE_NAME}" patterns in strings.
// In Go, regexp.MustCompile panics if the pattern is invalid — but since
// this is a compile-time constant, that's fine. It's a common Go pattern
// for package-level regex: compile once, use many times.
var envVarPattern = regexp.MustCompile(`\$\{([^}]+)\}`)

// Load reads a YAML config file, expands any ${ENV_VAR} references,
// and returns a parsed Config struct.
//
// Defaults come from config.yaml.example (looked up relative to the config
// file's directory). The user's config is layered on top — any field they
// don't set keeps the example file's default value.
func Load(path string) (*Config, error) {
	// Step 1: Load defaults from config.yaml.example.
	// We look for it in the same directory as the user's config file.
	// filepath.Dir extracts the directory part of a path — like Python's
	// os.path.dirname(). filepath.Join is os.path.join().
	dir := filepath.Dir(path)
	defaultsPath := filepath.Join(dir, "config.yaml.example")

	var cfg Config

	// Load the example file as our defaults baseline. If it's missing,
	// we just start with Go's zero values — the bot will still work as
	// long as the user's config has the required fields.
	if defaultsData, err := os.ReadFile(defaultsPath); err == nil {
		// We DON'T expand env vars in the example file — it contains
		// literal "${TELEGRAM_BOT_TOKEN}" placeholders that should stay
		// as empty strings (their env vars won't be set to those literals).
		if err := yaml.Unmarshal(defaultsData, &cfg); err != nil {
			return nil, fmt.Errorf("parsing defaults from %s: %w", defaultsPath, err)
		}
		// Clear out the token/key fields — the example file has "${...}"
		// placeholders that we don't want as actual defaults.
		cfg.Telegram.Token = ""
		cfg.LLM.APIKey = ""
	}

	// Step 2: Load the user's config on top.
	// yaml.Unmarshal only overwrites fields present in the YAML — fields
	// the user omits keep whatever value they had from step 1 (the defaults).
	// This is the key trick: two Unmarshal calls into the same struct.
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading config file: %w", err)
	}

	// Expand ${ENV_VAR} references in the user's config.
	expanded := expandEnvVars(string(data))

	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return nil, fmt.Errorf("parsing config YAML: %w", err)
	}

	return &cfg, nil
}

// SetTrace toggles the agent.trace field in both the in-memory config
// and the config.yaml file on disk. This does a surgical text edit —
// finds the "trace:" line under the "agent:" section and flips the value,
// preserving all comments and formatting. If the line doesn't exist yet,
// it gets inserted after max_tokens.
//
// This is intentionally narrow — a general-purpose YAML updater would
// be complex and fragile. Since we only need to toggle one boolean,
// a line-level edit is the pragmatic choice.
func (c *Config) SetTrace(configPath string, enabled bool) error {
	c.Driver.Trace = enabled

	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("reading config: %w", err)
	}

	lines := strings.Split(string(data), "\n")
	inAgent := false
	traceFound := false
	insertAfter := -1 // line index to insert after if trace: not found

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Detect top-level YAML sections (no leading whitespace).
		if len(trimmed) > 0 && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") && !strings.HasPrefix(trimmed, "#") {
			if strings.HasPrefix(trimmed, "agent:") {
				inAgent = true
			} else if inAgent {
				// We've left the agent section without finding trace:.
				break
			}
		}

		if inAgent {
			if strings.Contains(trimmed, "max_tokens:") {
				insertAfter = i
			}
			if strings.Contains(trimmed, "trace:") {
				// Found it — replace the value, preserving indent and comment.
				prefix := line[:strings.Index(line, "trace:")]
				val := "false"
				if enabled {
					val = "true"
				}
				lines[i] = prefix + "trace: " + val
				traceFound = true
				break
			}
		}
	}

	// If trace: wasn't found, insert it after max_tokens (or at end of agent section).
	if !traceFound && insertAfter >= 0 {
		indent := "  " // match agent section indentation
		val := "false"
		if enabled {
			val = "true"
		}
		newLine := indent + "trace: " + val
		// Insert after insertAfter index.
		lines = append(lines[:insertAfter+1], append([]string{newLine}, lines[insertAfter+1:]...)...)
	}

	return os.WriteFile(configPath, []byte(strings.Join(lines, "\n")), 0644)
}

// SetLocation writes the user's home coordinates (and optional display
// name) into both the in-memory config and config.yaml on disk. Like
// SetTrace, this is a surgical line-level edit — finds the latitude/
// longitude/name lines under the "location:" section and replaces
// their values, preserving comments and formatting.
//
// If the "location:" section doesn't exist yet, it's appended to the
// end of the file. We intentionally don't use a full YAML round-trip
// (yaml.Marshal → yaml.Unmarshal) because it would strip comments
// from the whole file.
func (c *Config) SetLocation(configPath string, lat, lon float64, name string) error {
	c.Location.Latitude = lat
	c.Location.Longitude = lon
	if name != "" {
		c.Location.Name = name
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("reading config: %w", err)
	}

	lines := strings.Split(string(data), "\n")
	const indent = "  " // same two-space indent as other top-level sections

	// Scan once to find the section and each field's line index (if present).
	inLocation := false
	locationHeader := -1
	latIdx, lonIdx, nameIdx := -1, -1, -1

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Detect top-level YAML sections (no leading whitespace, not a comment).
		isTopLevel := len(trimmed) > 0 &&
			!strings.HasPrefix(line, " ") &&
			!strings.HasPrefix(line, "\t") &&
			!strings.HasPrefix(trimmed, "#")

		if isTopLevel {
			if strings.HasPrefix(trimmed, "location:") {
				inLocation = true
				locationHeader = i
				continue
			}
			if inLocation {
				// Left the location: section — stop scanning.
				inLocation = false
			}
		}

		if inLocation {
			switch {
			case strings.HasPrefix(trimmed, "latitude:"):
				latIdx = i
			case strings.HasPrefix(trimmed, "longitude:"):
				lonIdx = i
			case strings.HasPrefix(trimmed, "name:"):
				nameIdx = i
			}
		}
	}

	// Helper: replace the "key: value" part of a line in place, preserving
	// leading indent and any trailing " # comment".
	replaceValue := func(line, key, value string) string {
		// Everything up to (and including) "key:".
		keyMarker := key + ":"
		keyPos := strings.Index(line, keyMarker)
		if keyPos < 0 {
			return line // shouldn't happen — we already matched the prefix
		}
		prefix := line[:keyPos]
		// Preserve an inline comment if there is one.
		rest := line[keyPos+len(keyMarker):]
		comment := ""
		if hashPos := strings.Index(rest, "#"); hashPos >= 0 {
			comment = " " + strings.TrimLeft(rest[hashPos:], " ")
		}
		return fmt.Sprintf("%s%s %s%s", prefix, keyMarker, value, comment)
	}

	// If the location section doesn't exist yet, append a fresh block.
	if locationHeader < 0 {
		block := []string{
			"",
			"location:",
			fmt.Sprintf("%slatitude: %s", indent, formatFloat(lat)),
			fmt.Sprintf("%slongitude: %s", indent, formatFloat(lon)),
		}
		if name != "" {
			block = append(block, fmt.Sprintf("%sname: %q", indent, name))
		}
		// Trim a trailing empty line so we don't end up with a double blank.
		if len(lines) > 0 && lines[len(lines)-1] == "" {
			lines = lines[:len(lines)-1]
		}
		lines = append(lines, block...)
		return os.WriteFile(configPath, []byte(strings.Join(lines, "\n")), 0644)
	}

	// Section exists — update in place, or insert any missing keys right
	// after the "location:" header. We insert in reverse order so the
	// index math stays correct after each splice.
	if nameIdx >= 0 && name != "" {
		lines[nameIdx] = replaceValue(lines[nameIdx], "name", fmt.Sprintf("%q", name))
	} else if name != "" {
		newLine := fmt.Sprintf("%sname: %q", indent, name)
		lines = insertAfter(lines, locationHeader, newLine)
		// Shift latIdx/lonIdx if they come after the header.
		if latIdx > locationHeader {
			latIdx++
		}
		if lonIdx > locationHeader {
			lonIdx++
		}
	}

	if lonIdx >= 0 {
		lines[lonIdx] = replaceValue(lines[lonIdx], "longitude", formatFloat(lon))
	} else {
		newLine := fmt.Sprintf("%slongitude: %s", indent, formatFloat(lon))
		lines = insertAfter(lines, locationHeader, newLine)
		if latIdx > locationHeader {
			latIdx++
		}
	}

	if latIdx >= 0 {
		lines[latIdx] = replaceValue(lines[latIdx], "latitude", formatFloat(lat))
	} else {
		newLine := fmt.Sprintf("%slatitude: %s", indent, formatFloat(lat))
		lines = insertAfter(lines, locationHeader, newLine)
	}

	return os.WriteFile(configPath, []byte(strings.Join(lines, "\n")), 0644)
}

// formatFloat renders a float with up to 6 decimal places and no trailing
// zeros — matches the precision Open-Meteo and Nominatim return. Using
// %g would drop trailing zeros but can slip into scientific notation for
// very small or very large values, which would be confusing in config.
func formatFloat(f float64) string {
	s := fmt.Sprintf("%.6f", f)
	s = strings.TrimRight(s, "0")
	s = strings.TrimRight(s, ".")
	if s == "" || s == "-" {
		return "0"
	}
	return s
}

// insertAfter splices a new line into a slice immediately after the given
// index. This is the standard Go idiom for inserting into a slice — no
// built-in "insert" function (unlike Python's list.insert()), so we build
// it from append() and a slice expression.
func insertAfter(lines []string, idx int, newLine string) []string {
	return append(lines[:idx+1], append([]string{newLine}, lines[idx+1:]...)...)
}

// ExportEnv sets process-level environment variables from config values
// so that skill subprocesses can find them via os.Getenv(). This bridges
// the gap between secrets stored in config.yaml and skills that check
// for env vars in their requirements.
//
// These vars only exist in the current process and its children — they
// never touch the parent shell. When the process exits (quit, signal,
// crash), they vanish automatically. No cleanup needed.
//
// Only sets a var if the config value is non-empty AND the env var isn't
// already set — we don't want to overwrite explicit env var exports.
func (c *Config) ExportEnv() {
	// Map of env var name → config value. Add new entries here
	// as skills declare new env requirements in their skill.md.
	exports := map[string]string{
		"TAVILY_API_KEY":     c.Search.TavilyAPIKey,
		"FOURSQUARE_API_KEY": c.Foursquare.APIKey,
		"OPENROUTER_API_KEY": c.LLM.APIKey,
		"TELEGRAM_BOT_TOKEN": c.Telegram.Token,
	}

	for key, val := range exports {
		if val == "" {
			continue // not configured — skip
		}
		if os.Getenv(key) != "" {
			continue // already set in shell — don't override
		}
		os.Setenv(key, val)
	}
}

// expandEnvVars replaces all ${VAR_NAME} patterns with their environment
// variable values. If a variable isn't set, the placeholder is replaced
// with an empty string.
func expandEnvVars(s string) string {
	return envVarPattern.ReplaceAllStringFunc(s, func(match string) string {
		// Extract the variable name from between ${ and }
		varName := strings.TrimPrefix(match, "${")
		varName = strings.TrimSuffix(varName, "}")
		return os.Getenv(varName)
	})
}
