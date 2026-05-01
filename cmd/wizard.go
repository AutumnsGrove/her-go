package cmd

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"

	"github.com/charmbracelet/huh"
	"gopkg.in/yaml.v3"
	"her/config"
)

// errWizardAborted is returned by runWizard when the user presses Ctrl+C.
// Callers treat this as a clean exit rather than an error condition.
var errWizardAborted = errors.New("wizard aborted")

// runWizard runs the interactive huh setup wizard, writing the result to
// cfgPath when the user completes all groups.
//
// Pre-population strategy:
//   - Non-sensitive fields are bound directly to the config struct, so
//     existing values are visible and editable inline.
//   - Sensitive fields (tokens, API keys) use a "leave blank to keep"
//     pattern: the input starts empty, and submitting empty preserves the
//     original value. The description shows "✓ Already set" when one exists,
//     so the user knows they can skip past it.
//
// If the user presses Ctrl+C at any point, nothing is written to disk.
func runWizard(cfgPath string) error {
	cfg, err := wizardLoadConfig(cfgPath)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Sensitive field buffers — each starts empty. After the form we apply
	// only the ones the user actually typed into. This is like Python's
	// "or existing" pattern: `new_val or old_val`.
	var (
		newTelegramToken   string
		newWebhookSecret   string
		newOpenRouterKey   string
		newTavilyKey       string
		newFoursquareKey   string
		newCloudflareToken string
	)

	// Int fields need intermediate strings because huh only binds *string.
	// strconv.FormatInt / strconv.Atoi are Go's explicit number↔string
	// converters — like Python's str() and int(), but typed.
	ownerChatStr := ""
	if cfg.Telegram.OwnerChat != 0 {
		ownerChatStr = strconv.FormatInt(cfg.Telegram.OwnerChat, 10)
	}
	webhookPortStr := ""
	if cfg.Telegram.WebhookPort != 0 {
		webhookPortStr = strconv.Itoa(cfg.Telegram.WebhookPort)
	}

	// Ensure fallback structs exist so huh can bind to their Model fields.
	// In Go, a nil pointer can't be dereferenced — unlike Python where
	// you'd just do `cfg.driver.fallback = cfg.driver.fallback or Fallback()`.
	// Here we must explicitly allocate the struct.
	if cfg.Driver.Fallback == nil {
		cfg.Driver.Fallback = &config.FallbackConfig{}
	}
	if cfg.Chat.Fallback == nil {
		cfg.Chat.Fallback = &config.FallbackConfig{}
	}
	if cfg.MemoryAgent.Fallback == nil {
		cfg.MemoryAgent.Fallback = &config.FallbackConfig{}
	}

	form := huh.NewForm(

		// ── Group 1: Identity ──────────────────────────────────────────────
		huh.NewGroup(
			huh.NewInput().
				Title("Bot name").
				Description("The bot's name — injected into prompts as {{her}}.").
				Value(&cfg.Identity.Her),

			huh.NewInput().
				Title("Your name").
				Description("Your name — injected into prompts as {{user}}.").
				Value(&cfg.Identity.User),

			huh.NewInput().
				Title("Owner chat ID").
				Description("Your Telegram chat ID — needed for scheduled reminders.\nSend /status to the bot after first run to find it.").
				Placeholder("optional — fill in after first run").
				Value(&ownerChatStr).
				Validate(validateOptionalInt64),
		).Title("Identity"),

		// ── Group 2: Telegram ──────────────────────────────────────────────
		huh.NewGroup(
			huh.NewInput().
				Title("Telegram bot token").
				Description("From @BotFather — required."+sensitiveHint(cfg.Telegram.Token)).
				EchoMode(huh.EchoModePassword).
				Value(&newTelegramToken).
				Validate(func(s string) error {
					if s == "" && cfg.Telegram.Token == "" {
						return fmt.Errorf("Telegram bot token is required")
					}
					return nil
				}),

			huh.NewSelect[string]().
				Title("Update mode").
				Description("How the bot receives Telegram messages.").
				Options(
					huh.NewOption("Poll  (long-poll — simple, no public URL needed)", "poll"),
					huh.NewOption("Webhook  (Telegram pushes updates — needs Cloudflare Tunnel)", "webhook"),
				).
				Value(&cfg.Telegram.Mode),

			huh.NewInput().
				Title("Webhook port").
				Description("Local HTTP port for the webhook server (ignored in poll mode).").
				Value(&webhookPortStr).
				Validate(validateOptionalPort),

			huh.NewInput().
				Title("Webhook secret").
				Description("Shared secret for X-Telegram-Bot-Api-Secret-Token header — optional."+sensitiveHint(cfg.Telegram.WebhookSecret)).
				EchoMode(huh.EchoModePassword).
				Value(&newWebhookSecret),
		).Title("Telegram"),

		// ── Group 3: API Keys ──────────────────────────────────────────────
		huh.NewGroup(
			huh.NewInput().
				Title("OpenRouter API key").
				Description("Powers all models via OpenRouter — required. Get one at openrouter.ai."+sensitiveHint(cfg.LLM.APIKey)).
				EchoMode(huh.EchoModePassword).
				Value(&newOpenRouterKey).
				Validate(func(s string) error {
					if s == "" && cfg.LLM.APIKey == "" {
						return fmt.Errorf("OpenRouter API key is required")
					}
					return nil
				}),

			huh.NewInput().
				Title("Tavily API key").
				Description("Enables web search — optional. Get one at tavily.com."+sensitiveHint(cfg.Search.TavilyAPIKey)).
				EchoMode(huh.EchoModePassword).
				Value(&newTavilyKey),

			huh.NewInput().
				Title("Foursquare API key").
				Description("Enables nearby place search — optional. Free tier: 10k calls/month."+sensitiveHint(cfg.Foursquare.APIKey)).
				EchoMode(huh.EchoModePassword).
				Value(&newFoursquareKey),
		).Title("API Keys"),

		// ── Group 4: Models ────────────────────────────────────────────────
		// All model names are pre-populated from config so most users can
		// just press Enter through this group. OpenRouter model IDs are
		// "provider/model-name" strings — no validation needed here.
		huh.NewGroup(
			huh.NewInput().
				Title("Driver / agent model").
				Description("Orchestrates tool calls each turn (think, recall, reply, done).").
				Value(&cfg.Driver.Model),

			huh.NewInput().
				Title("Chat / reply model").
				Description("Generates the actual user-facing response.").
				Value(&cfg.Chat.Model),

			huh.NewInput().
				Title("Memory agent model").
				Description("Extracts facts from conversation — runs in background after each reply.").
				Value(&cfg.MemoryAgent.Model),

			huh.NewInput().
				Title("Vision model").
				Description("Describes images sent to the bot (VLM).").
				Value(&cfg.Vision.Model),

			huh.NewInput().
				Title("Mood agent model").
				Description("Infers mood from conversation — runs in background after each reply.").
				Value(&cfg.MoodAgent.Model),

			huh.NewInput().
				Title("Classifier model").
				Description("Memory + reply safety gate — needs a fast, cheap model.").
				Value(&cfg.Classifier.Model),
		).Title("Models"),

		// ── Group 4b: Fallback Models ─────────────────────────────────────
		// Fallback models kick in when the primary model fails (rate limit,
		// timeout, server error). Pre-populated from config — usually a
		// cheaper/faster model as a safety net. Leave blank to disable.
		huh.NewGroup(
			huh.NewInput().
				Title("Driver fallback model").
				Description("Used when the primary driver model is unavailable.").
				Placeholder("optional — leave blank to disable").
				Value(&cfg.Driver.Fallback.Model),

			huh.NewInput().
				Title("Chat fallback model").
				Description("Used when the primary chat model is unavailable.").
				Placeholder("optional — leave blank to disable").
				Value(&cfg.Chat.Fallback.Model),

			huh.NewInput().
				Title("Memory agent fallback model").
				Description("Used when the primary memory model is unavailable.").
				Placeholder("optional — leave blank to disable").
				Value(&cfg.MemoryAgent.Fallback.Model),
		).Title("Fallback Models"),

		// ── Group 5: Cloudflare ────────────────────────────────────────────
		// All optional — only needed for webhook mode and cross-machine sync.
		huh.NewGroup(
			huh.NewInput().
				Title("Account ID").
				Description("Cloudflare account ID — found in the dashboard URL\n(/accounts/<id>/...). Only needed for webhook mode.").
				Value(&cfg.Cloudflare.AccountID),

			huh.NewInput().
				Title("API token").
				Description("Workers KV + D1 write permission — optional."+sensitiveHint(cfg.Cloudflare.APIToken)).
				EchoMode(huh.EchoModePassword).
				Value(&newCloudflareToken),

			huh.NewInput().
				Title("KV namespace ID").
				Description("From `npx wrangler kv namespace create` — optional.").
				Value(&cfg.Cloudflare.KVNamespaceID),

			huh.NewInput().
				Title("D1 database ID").
				Description("Enables cross-machine sync — leave empty to disable.").
				Value(&cfg.Cloudflare.D1DatabaseID),
		).Title("Cloudflare"),

		// ── Group 6: Infrastructure ────────────────────────────────────────
		huh.NewGroup(
			huh.NewInput().
				Title("Repo path").
				Description("Absolute path to the her-go directory.\nUsed by the /update self-update command.").
				Value(&cfg.Update.RepoPath),

			huh.NewInput().
				Title("Service label").
				Description("launchd service label — leave blank to auto-derive from bot name.").
				Value(&cfg.Update.ServiceLabel),

			huh.NewInput().
				Title("Tunnel name").
				Description("Cloudflare Tunnel name from `cloudflared tunnel create` — optional.").
				Value(&cfg.Tunnel.Name),

			huh.NewInput().
				Title("Tunnel domain").
				Description("Public hostname routed through this tunnel\n(e.g. her.yourdomain.com) — optional.").
				Value(&cfg.Tunnel.Domain),
		).Title("Infrastructure"),

		// ── Group 7: Features ──────────────────────────────────────────────
		huh.NewGroup(
			huh.NewConfirm().
				Title("Enable voice").
				Description("Accept voice memos from Telegram (requires parakeet-mlx setup).").
				Value(&cfg.Voice.Enabled),

			huh.NewConfirm().
				Title("Enable thinking traces").
				Description("Show agent reasoning in Telegram before each reply.\nToggle any time with /traces.").
				Value(&cfg.Driver.Trace),

			huh.NewConfirm().
				Title("Enable PII scrubbing").
				Description("Tokenize contact info in messages before sending to the LLM.").
				Value(&cfg.Scrub.Enabled),

			huh.NewInput().
				Title("Embed server URL").
				Description("Ollama or OpenAI-compatible embedding server URL.").
				Value(&cfg.Embed.BaseURL),
		).Title("Features"),
	)

	if err := form.Run(); err != nil {
		if errors.Is(err, huh.ErrUserAborted) {
			fmt.Println("\nSetup wizard cancelled — config not written.")
			return errWizardAborted
		}
		return fmt.Errorf("wizard: %w", err)
	}

	// Apply sensitive fields: non-empty new value replaces the original.
	// Empty means the user pressed Enter without typing — original is kept.
	applySensitive(&cfg.Telegram.Token, newTelegramToken)
	applySensitive(&cfg.Telegram.WebhookSecret, newWebhookSecret)
	applySensitive(&cfg.LLM.APIKey, newOpenRouterKey)
	applySensitive(&cfg.Search.TavilyAPIKey, newTavilyKey)
	applySensitive(&cfg.Foursquare.APIKey, newFoursquareKey)
	applySensitive(&cfg.Cloudflare.APIToken, newCloudflareToken)

	// Parse int fields back from their string representations.
	// Errors are safe to ignore here — both validators already accepted these values.
	if ownerChatStr != "" {
		cfg.Telegram.OwnerChat, _ = strconv.ParseInt(ownerChatStr, 10, 64)
	}
	if webhookPortStr != "" {
		cfg.Telegram.WebhookPort, _ = strconv.Atoi(webhookPortStr)
	}

	// Nil out fallback structs with empty models so they don't write
	// an empty `fallback: {model: ""}` block to config.yaml. In Go,
	// a nil pointer omits the field entirely from YAML output (via the
	// `omitempty` tag on the parent struct field).
	if cfg.Driver.Fallback != nil && cfg.Driver.Fallback.Model == "" {
		cfg.Driver.Fallback = nil
	}
	if cfg.Chat.Fallback != nil && cfg.Chat.Fallback.Model == "" {
		cfg.Chat.Fallback = nil
	}
	if cfg.MemoryAgent.Fallback != nil && cfg.MemoryAgent.Fallback.Model == "" {
		cfg.MemoryAgent.Fallback = nil
	}

	// Auto-derive service label from bot name if left blank.
	if cfg.Update.ServiceLabel == "" {
		cfg.Update.ServiceLabel = serviceLabel(cfg.Identity.Her)
	}

	if err := saveConfig(cfg, cfgPath); err != nil {
		return err
	}
	fmt.Printf("\nConfig written → %s\n", cfgPath)
	return nil
}

// applySensitive writes newVal into *target only when newVal is non-empty.
// An empty newVal means "keep existing" — the user pressed Enter without typing.
func applySensitive(target *string, newVal string) {
	if newVal != "" {
		*target = newVal
	}
}

// sensitiveHint returns a description suffix that signals an existing value
// is already configured, so the user knows they can press Enter to skip.
// The checkmark and "leave blank to keep" text make the pattern self-documenting.
func sensitiveHint(existing string) string {
	if existing != "" {
		return "\n✓ Already set — leave blank to keep, or type to replace."
	}
	return ""
}

// wizardLoadConfig loads the existing config for wizard pre-population.
// If config.yaml doesn't exist yet, it bootstraps from config.yaml.example
// so the wizard starts with sensible defaults rather than Go zero values.
func wizardLoadConfig(cfgPath string) (*config.Config, error) {
	cfg, err := config.Load(cfgPath)
	if err == nil {
		return cfg, nil
	}
	// errors.Is unwraps the fmt.Errorf("...: %w", err) chain from config.Load.
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	// No config.yaml yet — start from example defaults.
	cfg = &config.Config{}
	examplePath := filepath.Join(filepath.Dir(cfgPath), "config.yaml.example")
	if data, readErr := os.ReadFile(examplePath); readErr == nil {
		// config.yaml.example is read without env expansion, so ${VAR}
		// literals land as raw strings in cfg. We unmarshal for the
		// structural defaults (model names, ports, tuning knobs), then
		// explicitly clear every field that uses a ${VAR} placeholder so
		// sensitiveHint doesn't show "✓ Already set" on a fresh install.
		if err := yaml.Unmarshal(data, cfg); err != nil {
			log.Warn("config.yaml.example is malformed — starting wizard with empty defaults", "err", err)
			cfg = &config.Config{}
		}
		cfg.Telegram.Token = ""
		cfg.LLM.APIKey = ""
		cfg.Search.TavilyAPIKey = ""
		cfg.Foursquare.APIKey = ""
	}
	return cfg, nil
}

// saveConfig marshals cfg to YAML and writes it to path.
// 0600 permissions restrict read/write to the owner — important since
// config.yaml contains API keys and bot tokens.
//
// Note: yaml.Marshal is a full round-trip — inline comments and custom
// formatting in an existing config.yaml are not preserved on re-run.
// Values are always correct; only hand-written annotations are lost.
func saveConfig(cfg *config.Config, path string) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshaling config: %w", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("writing config: %w", err)
	}
	return nil
}

// validateOptionalInt64 accepts an empty string or a valid int64.
// Owner chat ID is optional at first-run — users might not know it yet.
func validateOptionalInt64(s string) error {
	if s == "" {
		return nil
	}
	if _, err := strconv.ParseInt(s, 10, 64); err != nil {
		return fmt.Errorf("must be a number (e.g. 123456789)")
	}
	return nil
}

// validateOptionalPort accepts an empty string or a valid TCP port (1–65535).
func validateOptionalPort(s string) error {
	if s == "" {
		return nil
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("must be a port number between 1 and 65535")
	}
	return nil
}
