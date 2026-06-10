// Package telegraph provides a client for the Telegraph API (telegra.ph),
// Telegram's built-in publishing platform. Reports written by the worker
// agent are published here for rich rendering with Instant View.
//
// The API is simple: create an account (one-time), then create pages.
// Pages are public and get automatic Instant View in Telegram clients.
//
// Telegraph supports: h3, h4, p, b, em, code, pre, blockquote, ul, ol,
// li, a, img, br, hr. No tables, no custom CSS, no H1/H2.
package telegraph

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"her/logger"
)

var log = logger.WithPrefix("telegraph")

const apiBase = "https://api.telegra.ph"

// Client holds the Telegraph API credentials and HTTP client.
type Client struct {
	accessToken string
	authorName  string
	httpClient  *http.Client
}

// NewClient creates a Telegraph client with the given access token.
// The token comes from CreateAccount (one-time setup) and is stored
// in config.yaml.
func NewClient(accessToken, authorName string) *Client {
	return &Client{
		accessToken: accessToken,
		authorName:  authorName,
		httpClient:  &http.Client{Timeout: 30 * time.Second},
	}
}

// CreateAccount creates a new Telegraph account and returns the access
// token. Call this once during setup, then store the token in config.
func CreateAccount(shortName, authorName string) (string, error) {
	payload := map[string]string{
		"short_name":  shortName,
		"author_name": authorName,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshaling request: %w", err)
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Post(apiBase+"/createAccount", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("creating account: %w", err)
	}
	defer resp.Body.Close()

	var result struct {
		OK     bool `json:"ok"`
		Result struct {
			AccessToken string `json:"access_token"`
		} `json:"result"`
		Error string `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decoding response: %w", err)
	}
	if !result.OK {
		return "", fmt.Errorf("telegraph API error: %s", result.Error)
	}

	token := result.Result.AccessToken
	prefix := token
	if len(token) > 8 {
		prefix = token[:8]
	}
	log.Info("telegraph account created", "token_prefix", prefix+"...")
	return result.Result.AccessToken, nil
}

// CreatePage publishes markdown content as a Telegraph page and returns
// the public URL. The markdown is converted to Telegraph's DOM node
// format automatically.
func (c *Client) CreatePage(title, markdownContent string) (string, error) {
	nodes := MarkdownToNodes(markdownContent)

	nodesJSON, err := json.Marshal(nodes)
	if err != nil {
		return "", fmt.Errorf("marshaling nodes: %w", err)
	}

	payload := map[string]string{
		"access_token": c.accessToken,
		"title":        title,
		"author_name":  c.authorName,
		"content":      string(nodesJSON),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshaling request: %w", err)
	}

	resp, err := c.httpClient.Post(apiBase+"/createPage", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", fmt.Errorf("creating page: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading response: %w", err)
	}

	var result struct {
		OK     bool `json:"ok"`
		Result struct {
			URL string `json:"url"`
		} `json:"result"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("decoding response: %w", err)
	}
	if !result.OK {
		return "", fmt.Errorf("telegraph API error: %s", result.Error)
	}

	log.Info("telegraph page created", "url", result.Result.URL)
	return result.Result.URL, nil
}
