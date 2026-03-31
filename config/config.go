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
	Identity  IdentityConfig  `yaml:"identity"`
	Telegram  TelegramConfig  `yaml:"telegram"`
	LLM       LLMConfig       `yaml:"llm"`
	Agent     AgentConfig     `yaml:"agent"`
	Vision     VisionConfig     `yaml:"vision"`
	Classifier ClassifierConfig `yaml:"classifier"`
	Memory     MemoryConfig     `yaml:"memory"`
	Embed     EmbedConfig     `yaml:"embed"`
	Search    SearchConfig    `yaml:"search"`
	Scrub     ScrubConfig     `yaml:"scrub"`
	Persona   PersonaConfig   `yaml:"persona"`
	Scheduler SchedulerConfig `yaml:"scheduler"`
	Voice     VoiceConfig     `yaml:"voice"`
	Weather   WeatherConfig   `yaml:"weather"`
	OCR       OCRConfig       `yaml:"ocr"`
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
type TelegramConfig struct {
	Token      string `yaml:"token"`
	Mode       string `yaml:"mode"`        // "poll" or "webhook"
	WebhookURL string `yaml:"webhook_url"` // only needed for webhook mode
	OwnerChat  int64  `yaml:"owner_chat"`  // chat ID for the bot owner — used by scheduler for proactive messages
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

// LLMConfig holds OpenRouter / OpenAI-compatible API settings.
type LLMConfig struct {
	BaseURL     string          `yaml:"base_url"`
	APIKey      string          `yaml:"api_key"`
	Model       string          `yaml:"model"`
	Temperature float64         `yaml:"temperature"`
	MaxTokens   int             `yaml:"max_tokens"`
	Fallback    *FallbackConfig `yaml:"fallback"` // optional fallback model for when primary is unavailable
}

// AgentConfig holds settings for the background tool-calling agent.
// This runs a separate, lightweight model (Liquid LFM) that handles
// memory management and can trigger follow-up messages.
type AgentConfig struct {
	Model       string          `yaml:"model"`
	Temperature float64         `yaml:"temperature"`
	MaxTokens   int             `yaml:"max_tokens"`
	Trace       bool            `yaml:"trace"`    // show agent thinking traces in chat
	Fallback    *FallbackConfig `yaml:"fallback"` // optional fallback model for when primary is unavailable
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

// MemoryConfig controls the SQLite-backed memory system.
type MemoryConfig struct {
	DBPath             string  `yaml:"db_path"`
	RecentMessages     int     `yaml:"recent_messages"`
	MaxFactsInContext  int     `yaml:"max_facts_in_context"`
	ExtractionInterval int     `yaml:"extraction_interval"`
	MaxHistoryTokens   int     `yaml:"max_history_tokens"`    // estimation-based compaction budget (len/4 heuristic over full history window)
	ChatContextBudget  int     `yaml:"chat_context_budget"`   // chat model total prompt budget (compaction fires at 75%); 0 = use max_history_tokens only
	MaxContextTokens   int     `yaml:"max_context_tokens"`    // DEPRECATED: use chat_context_budget instead. Kept for backwards compat.
	AutoLinkCount      int     `yaml:"auto_link_count"`       // max links per new fact (0 = disabled)
	AutoLinkThreshold  float64 `yaml:"auto_link_threshold"`   // min cosine similarity to create a link (0.0-1.0)
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
	Dimension           int     `yaml:"dimension"`             // vector dimension (768 for nomic-embed-text-v1.5, 1536 for OpenAI, etc.)
	SimilarityThreshold float64 `yaml:"similarity_threshold"`  // above this = duplicate (0.0-1.0)
	MaxSemanticDistance float64 `yaml:"max_semantic_distance"` // facts farther than this are filtered from context (cosine distance, 0=identical)
}

// SearchConfig controls web search and book search integrations.
type SearchConfig struct {
	TavilyAPIKey  string `yaml:"tavily_api_key"`
	TavilyBaseURL string `yaml:"tavily_base_url"` // defaults to https://api.tavily.com
}

// PersonaConfig controls the persona evolution system.
type PersonaConfig struct {
	PromptFile                string  `yaml:"prompt_file"`
	PersonaFile               string  `yaml:"persona_file"`
	AgentPromptFile           string  `yaml:"agent_prompt_file"`
	RewriteEveryNReflections  int     `yaml:"rewrite_every_n_reflections"`
	ReflectionMemoryThreshold int     `yaml:"reflection_memory_threshold"`
	MaxTraitShift             float64 `yaml:"max_trait_shift"`
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

// TTSConfig controls text-to-speech. The "kokoro" engine expects a local
// HTTP server (mlx-audio) running on BaseURL with an OpenAI-compatible
// /v1/audio/speech endpoint. The Go side POSTs JSON and gets back WAV bytes.
type TTSConfig struct {
	Enabled   bool    `yaml:"enabled"`
	Engine    string  `yaml:"engine"`     // "kokoro" or future options
	BaseURL   string  `yaml:"base_url"`   // e.g. "http://localhost:8766"
	Model     string  `yaml:"model"`      // HuggingFace model ID or local path
	VoiceID   string  `yaml:"voice_id"`   // voice preset (e.g. "af_heart", "af_nova")
	Speed     float64 `yaml:"speed"`      // speaking rate (1.0 = normal)
	ReplyMode string  `yaml:"reply_mode"` // "voice" (always reply with voice) or "match" (mirror input format)
}

// SchedulerConfig controls the task scheduler / cron system.
type SchedulerConfig struct {
	Timezone           string `yaml:"timezone"`              // IANA timezone for cron evaluation (e.g. "America/New_York")
	QuietHoursStart    string `yaml:"quiet_hours_start"`     // no scheduled messages after this time (e.g. "23:00")
	QuietHoursEnd      string `yaml:"quiet_hours_end"`       // resume scheduled messages after this time (e.g. "07:00")
	MaxProactivePerDay int    `yaml:"max_proactive_per_day"` // cap on non-reminder messages per day (0 = unlimited)

	// Default task flags — when true, the scheduler creates these tasks
	// on startup if they don't already exist. All are idempotent.
	MorningBriefing    bool `yaml:"morning_briefing"`    // daily briefing at 8am via run_prompt
	MoodCheckin        bool `yaml:"mood_checkin"`        // daily mood check-in at 9pm
	MedicationCheckin  bool `yaml:"medication_checkin"`  // daily medication check-in at 9pm (critical priority)
	ProactiveFollowups bool `yaml:"proactive_followups"` // scan for follow-up opportunities at 9am
	AutoJournal        bool `yaml:"auto_journal"`        // auto-journal entry at 10pm
}

// WeatherConfig controls the Open-Meteo weather integration.
// Weather data is fetched periodically and injected into the system
// prompt as environmental context. No API key needed — Open-Meteo is free.
type WeatherConfig struct {
	Latitude      float64 `yaml:"latitude"`        // WGS84 latitude (e.g., 40.7128 for New York)
	Longitude     float64 `yaml:"longitude"`       // WGS84 longitude (e.g., -74.0060 for New York)
	TempUnit      string  `yaml:"temp_unit"`       // "fahrenheit" (default) or "celsius"
	WindSpeedUnit string  `yaml:"wind_speed_unit"` // "mph" (default) or "kmh"
	CacheTTL      int     `yaml:"cache_ttl"`       // seconds between API calls (default: 3600 = 1 hour)
}

// OCRConfig controls the local OCR pipeline used for pre-flight text
// extraction on photos. The primary engine is Apple Vision (via the
// macos-vision-ocr CLI binary) — it runs on the Neural Engine, sub-200ms,
// zero dependencies. If confidence is low or the binary isn't available,
// falls back to GLM-OCR via LM Studio (a purpose-built OCR model).
//
// This runs on EVERY incoming photo as a "pre-flight" check before the
// agent decides what to do. Since it's local, it's essentially free.
type OCRConfig struct {
	VisionOCRPath       string      `yaml:"vision_ocr_path"`      // path to macos-vision-ocr binary (or just the name if it's on PATH)
	ConfidenceThreshold float64     `yaml:"confidence_threshold"` // below this average confidence → fall back to GLM-OCR (0.0-1.0)
	Fallback            OCRFallback `yaml:"fallback"`
}

// OCRFallback configures the secondary OCR engine (GLM-OCR via LM Studio).
// Used when Apple Vision returns low confidence or empty results — e.g.,
// receipts with unusual fonts, heavy glare, or non-Latin scripts.
type OCRFallback struct {
	Engine  string `yaml:"engine"`   // "glm-ocr" (only option for now)
	BaseURL string `yaml:"base_url"` // LM Studio endpoint (e.g., "http://localhost:1234/v1")
	Model   string `yaml:"model"`    // model name in LM Studio (e.g., "glm-ocr")
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

	// Backwards compat: if the user still has max_context_tokens set but
	// hasn't migrated to chat_context_budget, use the old value.
	if cfg.Memory.ChatContextBudget == 0 && cfg.Memory.MaxContextTokens > 0 {
		cfg.Memory.ChatContextBudget = cfg.Memory.MaxContextTokens
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
	c.Agent.Trace = enabled

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

// SetLocation updates the weather latitude and longitude in both the
// in-memory config and the config.yaml file on disk. Like SetTrace,
// it does a surgical line edit — finds the latitude: and longitude:
// lines under the weather: section and updates them in place, or
// inserts them after the weather: line if they don't exist yet.
//
// This is the persistence half of set_location — the weather client
// is updated in memory by its own SetLocation method, and this call
// makes sure the new coordinates survive a restart.
func (c *Config) SetLocation(configPath string, lat, lon float64) error {
	c.Weather.Latitude = lat
	c.Weather.Longitude = lon

	data, err := os.ReadFile(configPath)
	if err != nil {
		return fmt.Errorf("reading config: %w", err)
	}

	lines := strings.Split(string(data), "\n")
	inWeather := false
	latFound := false
	lonFound := false
	weatherLineIdx := -1 // index of the "weather:" line itself, for fallback insertion

	for i, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Detect top-level YAML sections (no leading whitespace).
		if len(trimmed) > 0 && !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "\t") && !strings.HasPrefix(trimmed, "#") {
			if strings.HasPrefix(trimmed, "weather:") {
				inWeather = true
				weatherLineIdx = i
			} else if inWeather {
				// Left the weather section — stop scanning.
				break
			}
		}

		if inWeather {
			if strings.Contains(trimmed, "latitude:") {
				prefix := line[:strings.Index(line, "latitude:")]
				lines[i] = fmt.Sprintf("%slatitude: %g", prefix, lat)
				latFound = true
			}
			if strings.Contains(trimmed, "longitude:") {
				prefix := line[:strings.Index(line, "longitude:")]
				lines[i] = fmt.Sprintf("%slongitude: %g", prefix, lon)
				lonFound = true
			}
		}
	}

	// If either line was missing, insert both after the weather: line.
	// We insert in reverse order so indices stay valid.
	indent := "  " // match weather section indentation
	if !lonFound {
		newLine := fmt.Sprintf("%slongitude: %g", indent, lon)
		lines = append(lines[:weatherLineIdx+1], append([]string{newLine}, lines[weatherLineIdx+1:]...)...)
	}
	if !latFound {
		newLine := fmt.Sprintf("%slatitude: %g", indent, lat)
		lines = append(lines[:weatherLineIdx+1], append([]string{newLine}, lines[weatherLineIdx+1:]...)...)
	}

	return os.WriteFile(configPath, []byte(strings.Join(lines, "\n")), 0644)
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
		"TAVILY_API_KEY":    c.Search.TavilyAPIKey,
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
