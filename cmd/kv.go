package cmd

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"text/template"
	"time"

	"her/config"
)

// kvClientTimeout is the HTTP timeout for Cloudflare KV API calls.
const kvClientTimeout = 10 * time.Second

// --- Cloudflare Workers KV client ---

type kvClient struct {
	accountID   string
	apiToken    string
	namespaceID string
	http        *http.Client
}

func (c *kvClient) kvBaseURL() string {
	return fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/storage/kv/namespaces/%s", c.accountID, c.namespaceID)
}

func (c *kvClient) get(key string) (string, error) {
	url := c.kvBaseURL() + "/values/" + key
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiToken)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("kv get %s: %w", key, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("kv get %s: HTTP %d: %s", key, resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("kv get %s: reading body: %w", key, err)
	}
	return string(body), nil
}

func (c *kvClient) put(key, value string) error {
	url := c.kvBaseURL() + "/values/" + key
	req, err := http.NewRequest("PUT", url, strings.NewReader(value))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiToken)
	req.Header.Set("Content-Type", "text/plain")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("kv put %s: %w", key, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("kv put %s: HTTP %d: %s", key, resp.StatusCode, string(body))
	}
	return nil
}

func (c *kvClient) delete(key string) error {
	url := c.kvBaseURL() + "/values/" + key
	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiToken)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("kv delete %s: %w", key, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("kv delete %s: HTTP %d: %s", key, resp.StatusCode, string(body))
	}
	return nil
}

func nowMillis() string {
	return strconv.FormatInt(time.Now().UnixMilli(), 10)
}

// ---------------------------------------------------------------------------
// Wrangler config generator
// ---------------------------------------------------------------------------

type wranglerData struct {
	KVNamespaceID string
	D1DatabaseID  string
	ProdURL       string
}

func generateWranglerConfig(cfg *config.Config) error {
	tmplPath := filepath.Join("worker", "wrangler.toml.tmpl")
	outPath := filepath.Join("worker", "wrangler.toml")

	tmplBytes, err := os.ReadFile(tmplPath)
	if err != nil {
		return fmt.Errorf("reading wrangler template: %w", err)
	}

	tmpl, err := template.New("wrangler").Parse(string(tmplBytes))
	if err != nil {
		return fmt.Errorf("parsing wrangler template: %w", err)
	}

	prodURL := ""
	if cfg.Tunnel.Domain != "" {
		prodURL = "https://" + cfg.Tunnel.Domain
	}

	data := wranglerData{
		KVNamespaceID: cfg.Cloudflare.KVNamespaceID,
		D1DatabaseID:  cfg.Cloudflare.D1DatabaseID,
		ProdURL:       prodURL,
	}

	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("creating wrangler.toml: %w", err)
	}
	defer f.Close()

	if err := tmpl.Execute(f, data); err != nil {
		return fmt.Errorf("writing wrangler.toml: %w", err)
	}

	log.Info("generated worker/wrangler.toml from config.yaml")
	return nil
}
