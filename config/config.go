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
	Telegram TelegramConfig `yaml:"telegram"`
	LLM      LLMConfig      `yaml:"llm"`
	Agent    AgentConfig    `yaml:"agent"`
	Memory   MemoryConfig   `yaml:"memory"`
	Embed    EmbedConfig    `yaml:"embed"`
	Scrub    ScrubConfig    `yaml:"scrub"`
	Persona  PersonaConfig  `yaml:"persona"`
}

// TelegramConfig holds Telegram bot settings.
type TelegramConfig struct {
	Token      string `yaml:"token"`
	Mode       string `yaml:"mode"`        // "poll" or "webhook"
	WebhookURL string `yaml:"webhook_url"` // only needed for webhook mode
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
}

// MemoryConfig controls the SQLite-backed memory system.
type MemoryConfig struct {
	DBPath             string `yaml:"db_path"`
	RecentMessages     int    `yaml:"recent_messages"`
	MaxFactsInContext  int    `yaml:"max_facts_in_context"`
	ExtractionInterval int    `yaml:"extraction_interval"`
}

// ScrubConfig controls PII scrubbing behavior.
type ScrubConfig struct {
	Enabled bool `yaml:"enabled"`
}

// EmbedConfig controls the local embedding model used for semantic
// similarity (memory deduplication, future vector search).
type EmbedConfig struct {
	BaseURL            string  `yaml:"base_url"`
	Model              string  `yaml:"model"`
	SimilarityThreshold float64 `yaml:"similarity_threshold"` // above this = duplicate (0.0-1.0)
}

// PersonaConfig controls the persona evolution system.
type PersonaConfig struct {
	PromptFile                 string  `yaml:"prompt_file"`
	PersonaFile                string  `yaml:"persona_file"`
	RewriteEveryNConversations int     `yaml:"rewrite_every_n_conversations"`
	ReflectionMemoryThreshold  int     `yaml:"reflection_memory_threshold"`
	MaxTraitShift              float64 `yaml:"max_trait_shift"`
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
