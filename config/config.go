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
	Telegram  TelegramConfig  `yaml:"telegram"`
	LLM       LLMConfig       `yaml:"llm"`
	Agent     AgentConfig     `yaml:"agent"`
	Vision    VisionConfig    `yaml:"vision"`
	Memory    MemoryConfig    `yaml:"memory"`
	Embed     EmbedConfig     `yaml:"embed"`
	Search    SearchConfig    `yaml:"search"`
	Scrub     ScrubConfig     `yaml:"scrub"`
	Persona   PersonaConfig   `yaml:"persona"`
	Scheduler SchedulerConfig `yaml:"scheduler"`
	Voice     VoiceConfig     `yaml:"voice"`
	Weather   WeatherConfig   `yaml:"weather"`
}

// TelegramConfig holds Telegram bot settings.
type TelegramConfig struct {
	Token      string `yaml:"token"`
	Mode       string `yaml:"mode"`        // "poll" or "webhook"
	WebhookURL string `yaml:"webhook_url"` // only needed for webhook mode
	OwnerChat  int64  `yaml:"owner_chat"`  // chat ID for the bot owner — used by scheduler for proactive messages
}

// LLMConfig holds OpenRouter / OpenAI-compatible API settings.
type LLMConfig struct {
	BaseURL     string  `yaml:"base_url"`
	APIKey      string  `yaml:"api_key"`
	Model       string  `yaml:"model"`
	Temperature float64 `yaml:"temperature"`
	MaxTokens   int     `yaml:"max_tokens"`
}

// AgentConfig holds settings for the background tool-calling agent.
// This runs a separate, lightweight model (Liquid LFM) that handles
// memory management and can trigger follow-up messages.
type AgentConfig struct {
	Model       string  `yaml:"model"`
	Temperature float64 `yaml:"temperature"`
	MaxTokens   int     `yaml:"max_tokens"`
	Trace       bool    `yaml:"trace"` // show agent thinking traces in chat
}

// VisionConfig holds settings for the vision language model (VLM).
// Used for image understanding — when the user sends a photo, the
// view_image agent tool calls this model to describe what's in it.
// Shares the same base_url and api_key as the main LLM section.
type VisionConfig struct {
	Model       string  `yaml:"model"`
	Temperature float64 `yaml:"temperature"`
	MaxTokens   int     `yaml:"max_tokens"`
}

// MemoryConfig controls the SQLite-backed memory system.
type MemoryConfig struct {
	DBPath             string `yaml:"db_path"`
	RecentMessages     int    `yaml:"recent_messages"`
	MaxFactsInContext  int    `yaml:"max_facts_in_context"`
	ExtractionInterval int    `yaml:"extraction_interval"`
	MaxHistoryTokens   int    `yaml:"max_history_tokens"`   // token budget for conversation history before compaction triggers
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
	Dimension           int     `yaml:"dimension"`            // vector dimension (768 for nomic-embed-text-v1.5, 1536 for OpenAI, etc.)
	SimilarityThreshold float64 `yaml:"similarity_threshold"` // above this = duplicate (0.0-1.0)
}

// SearchConfig controls web search and book search integrations.
type SearchConfig struct {
	TavilyAPIKey string `yaml:"tavily_api_key"`
	TavilyBaseURL string `yaml:"tavily_base_url"` // defaults to https://api.tavily.com
}

// PersonaConfig controls the persona evolution system.
type PersonaConfig struct {
	PromptFile                 string  `yaml:"prompt_file"`
	PersonaFile                string  `yaml:"persona_file"`
	AgentPromptFile            string  `yaml:"agent_prompt_file"`
	RewriteEveryNConversations int     `yaml:"rewrite_every_n_conversations"`
	ReflectionMemoryThreshold  int     `yaml:"reflection_memory_threshold"`
	MaxTraitShift              float64 `yaml:"max_trait_shift"`
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
	MorningBriefing    bool `yaml:"morning_briefing"`     // daily briefing at 8am via run_prompt
	MoodCheckin        bool `yaml:"mood_checkin"`          // daily mood check-in at 9pm
	MedicationCheckin  bool `yaml:"medication_checkin"`    // daily medication check-in at 9pm (critical priority)
	ProactiveFollowups bool `yaml:"proactive_followups"`   // scan for follow-up opportunities at 9am
	AutoJournal        bool `yaml:"auto_journal"`          // auto-journal entry at 10pm
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
