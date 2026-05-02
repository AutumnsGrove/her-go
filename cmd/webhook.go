package cmd

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"her/config"
)

// workerURLPattern matches the workers.dev URL that wrangler prints on deploy.
// Output looks like: "Published her-router (0.5 sec)\n  https://her-router.xxx.workers.dev"
var workerURLPattern = regexp.MustCompile(`https://[a-zA-Z0-9-]+\.[a-zA-Z0-9-]+\.workers\.dev`)

// deployWebhook handles the full webhook setup lifecycle:
//  1. Generate wrangler.toml from config
//  2. Deploy the CF Worker via wrangler
//  3. Set the WEBHOOK_SECRET as a wrangler secret
//  4. Register the webhook with Telegram's setWebhook API
//  5. Save the Worker URL to config.yaml
//
// This is the glue that connects all the pieces — without it, the user
// has to manually deploy, set secrets, and curl Telegram. Now it's one
// function call from `her setup`.
func deployWebhook(cfg *config.Config, cfgPath string) error {
	// Validate prerequisites.
	if cfg.Telegram.Token == "" {
		return fmt.Errorf("telegram token is required for webhook setup")
	}
	if cfg.Telegram.WebhookSecret == "" {
		return fmt.Errorf("webhook_secret is required — generate one with: openssl rand -hex 32")
	}
	if cfg.Cloudflare.KVNamespaceID == "" {
		return fmt.Errorf("cloudflare.kv_namespace_id is required — create one with: npx wrangler kv namespace create HER_KV")
	}

	// Check wrangler is available.
	if _, err := exec.LookPath("npx"); err != nil {
		return fmt.Errorf("npx not found — install Node.js to use wrangler")
	}

	// Step 1: Generate wrangler.toml from config template.
	log.Info("[webhook 1/4] generating wrangler.toml")
	if err := generateWranglerConfig(cfg); err != nil {
		return fmt.Errorf("generating wrangler.toml: %w", err)
	}

	// Step 2: Deploy the Worker.
	log.Info("[webhook 2/4] deploying CF Worker")
	workerURL, err := wranglerDeploy()
	if err != nil {
		return fmt.Errorf("wrangler deploy: %w", err)
	}
	log.Info("worker deployed", "url", workerURL)

	// Step 3: Set the webhook secret in wrangler secrets.
	// We pipe it to stdin because wrangler secret put reads from stdin
	// when not running interactively.
	log.Info("[webhook 3/4] setting WEBHOOK_SECRET")
	if err := wranglerSetSecret("WEBHOOK_SECRET", cfg.Telegram.WebhookSecret); err != nil {
		return fmt.Errorf("setting webhook secret: %w", err)
	}

	// Step 4: Register the webhook with Telegram.
	log.Info("[webhook 4/4] registering webhook with Telegram")
	if err := telegramSetWebhook(cfg.Telegram.Token, workerURL, cfg.Telegram.WebhookSecret); err != nil {
		return fmt.Errorf("registering webhook: %w", err)
	}

	// Save the Worker URL to config so we can verify it on future startups.
	cfg.Telegram.WebhookURL = workerURL
	if err := saveConfig(cfg, cfgPath); err != nil {
		return fmt.Errorf("saving webhook_url to config: %w", err)
	}

	log.Info("webhook setup complete",
		"worker_url", workerURL,
		"telegram", "registered",
	)
	return nil
}

// wranglerDeploy runs `npx wrangler deploy` in the worker/ directory and
// parses the Worker URL from the output. The URL is printed on successful
// deploy like: "https://her-router.xxx.workers.dev"
func wranglerDeploy() (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "npx", "wrangler", "deploy")
	cmd.Dir = "worker"
	// Combine stdout+stderr — wrangler prints the URL to stdout but
	// progress/errors to stderr.
	out, err := cmd.CombinedOutput()
	output := string(out)

	if err != nil {
		return "", fmt.Errorf("%w\n%s", err, output)
	}

	// Parse the workers.dev URL from the output.
	match := workerURLPattern.FindString(output)
	if match == "" {
		// Fallback: if the user has a custom domain route, wrangler may
		// not print a workers.dev URL. Check if we got a "Published" line.
		if strings.Contains(output, "Published") || strings.Contains(output, "Uploaded") {
			// Worker deployed but no workers.dev URL — probably using
			// routes. The user needs to tell us the URL.
			return "", fmt.Errorf("worker deployed but could not parse URL from output:\n%s\nSet telegram.webhook_url manually in config.yaml", output)
		}
		return "", fmt.Errorf("deploy succeeded but no URL found in output:\n%s", output)
	}

	return match, nil
}

// wranglerSetSecret pipes a secret value to `npx wrangler secret put`.
// Wrangler reads from stdin when not attached to a TTY — we write the
// value and close the pipe so it completes immediately.
func wranglerSetSecret(name, value string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "npx", "wrangler", "secret", "put", name)
	cmd.Dir = "worker"
	cmd.Stdin = strings.NewReader(value)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// telegramSetWebhook calls the Telegram Bot API to register our Worker
// as the webhook endpoint. The secret_token is sent with setWebhook and
// Telegram includes it in every update POST as X-Telegram-Bot-Api-Secret-Token.
func telegramSetWebhook(token, webhookURL, secret string) error {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/setWebhook", token)

	params := url.Values{}
	params.Set("url", webhookURL)
	if secret != "" {
		params.Set("secret_token", secret)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.PostForm(apiURL, params)
	if err != nil {
		return fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("telegram API HTTP %d: %s", resp.StatusCode, string(body))
	}

	body, _ := io.ReadAll(resp.Body)

	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("parsing response: %w (body: %s)", err, string(body))
	}

	if !result.OK {
		return fmt.Errorf("Telegram API error: %s", result.Description)
	}

	log.Info("webhook registered with Telegram", "url", webhookURL)
	return nil
}

// telegramGetWebhookInfo calls getWebhookInfo to check the current state.
type webhookInfo struct {
	URL                string `json:"url"`
	PendingUpdateCount int    `json:"pending_update_count"`
	HasCustomCert      bool   `json:"has_custom_certificate"`
	LastErrorDate      int64  `json:"last_error_date"`
	LastErrorMessage   string `json:"last_error_message"`
}

func telegramGetWebhookInfo(token string) (*webhookInfo, error) {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/getWebhookInfo", token)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(apiURL)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("telegram API HTTP %d: %s", resp.StatusCode, string(body))
	}

	body, _ := io.ReadAll(resp.Body)

	var response struct {
		OK     bool        `json:"ok"`
		Result webhookInfo `json:"result"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}

	if !response.OK {
		return nil, fmt.Errorf("Telegram API returned ok=false")
	}

	return &response.Result, nil
}

// ensureWebhookSecret generates a webhook secret if one doesn't exist.
// Returns true if a new secret was generated.
func ensureWebhookSecret(cfg *config.Config) bool {
	if cfg.Telegram.WebhookSecret != "" {
		return false
	}
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return false
	}
	cfg.Telegram.WebhookSecret = fmt.Sprintf("%x", buf)
	return true
}
