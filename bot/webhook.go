package bot

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"
)

// verifyWebhookRegistration checks that Telegram's webhook points at our
// CF Worker URL and re-registers if it's missing or stale. This runs on
// every bot startup in webhook mode — it's cheap (one HTTP GET) and
// self-heals from manual clearings, failed deploys, or the bot having
// previously run in poll mode (which calls RemoveWebhook).
func (b *Bot) verifyWebhookRegistration() error {
	expectedURL := b.cfg.Telegram.WebhookURL
	if expectedURL == "" {
		log.Warn("webhook_url is empty — run `her setup` with webhook mode to deploy the Worker")
		return nil
	}

	// Ask Telegram what webhook it has registered.
	info, err := b.getWebhookInfo()
	if err != nil {
		return fmt.Errorf("checking webhook: %w", err)
	}

	if info.url == expectedURL {
		log.Info("webhook verified",
			"url", info.url,
			"pending_updates", info.pendingUpdates,
		)
		if info.lastError != "" {
			log.Warn("telegram reports webhook errors",
				"last_error", info.lastError,
			)
		}
		return nil
	}

	// Mismatch — re-register.
	log.Warn("webhook URL mismatch, re-registering",
		"expected", expectedURL,
		"actual", info.url,
	)
	return b.setWebhook(expectedURL, b.cfg.Telegram.WebhookSecret)
}

type telegramWebhookInfo struct {
	url            string
	pendingUpdates int
	lastError      string
}

func (b *Bot) getWebhookInfo() (*telegramWebhookInfo, error) {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/getWebhookInfo", b.cfg.Telegram.Token)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(apiURL)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	var response struct {
		OK     bool `json:"ok"`
		Result struct {
			URL                string `json:"url"`
			PendingUpdateCount int    `json:"pending_update_count"`
			LastErrorMessage   string `json:"last_error_message"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &response); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}

	return &telegramWebhookInfo{
		url:            response.Result.URL,
		pendingUpdates: response.Result.PendingUpdateCount,
		lastError:      response.Result.LastErrorMessage,
	}, nil
}

func (b *Bot) setWebhook(webhookURL, secret string) error {
	apiURL := fmt.Sprintf("https://api.telegram.org/bot%s/setWebhook", b.cfg.Telegram.Token)

	params := url.Values{}
	params.Set("url", webhookURL)
	if secret != "" {
		params.Set("secret_token", secret)
	}

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.PostForm(apiURL, params)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, _ := io.ReadAll(resp.Body)

	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return fmt.Errorf("parsing response: %w", err)
	}
	if !result.OK {
		return fmt.Errorf("Telegram API: %s", result.Description)
	}

	log.Info("webhook re-registered with Telegram", "url", webhookURL)
	return nil
}
