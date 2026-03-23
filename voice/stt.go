// Package voice handles speech-to-text (v0.3) and text-to-speech (v0.5).
//
// STT architecture: a Python sidecar (parakeet-mlx-fastapi) runs locally
// and exposes an OpenAI-compatible /v1/audio/transcriptions endpoint.
// This package is the Go HTTP client that talks to it. No Python bindings
// needed — just multipart/form-data over HTTP.
//
// The sidecar approach is necessary because Parakeet runs on MLX (Apple's
// ML framework), which only has Python/Swift bindings. The HTTP boundary
// keeps our Go code clean and the ML stuff in its natural habitat.
package voice

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"her/config"
	"her/logger"
)

var log = logger.WithPrefix("voice")

// Client is the STT client that talks to the local parakeet-server.
// It holds the base URL and model name from config, and a reusable
// HTTP client with a generous timeout (transcription can take a few
// seconds for longer voice memos).
type Client struct {
	baseURL    string
	model      string
	httpClient *http.Client
}

// NewClient creates a new STT client from config. If voice is disabled
// or the base URL is empty, returns nil — callers should check for nil
// before using.
//
// This is a common Go pattern: "constructor" functions that return a
// pointer (or nil if not needed). Similar to Python's __init__ returning
// None to signal "don't use this", except in Go you return nil explicitly
// and the caller is expected to check.
func NewClient(cfg *config.VoiceConfig) *Client {
	if !cfg.Enabled {
		return nil
	}

	baseURL := strings.TrimRight(cfg.STT.BaseURL, "/")
	if baseURL == "" {
		log.Warn("voice enabled but stt.base_url is empty — STT disabled")
		return nil
	}

	log.Info("STT client initialized",
		"engine", cfg.STT.Engine,
		"base_url", baseURL,
		"model", cfg.STT.Model,
	)

	return &Client{
		baseURL: baseURL,
		model:   cfg.STT.Model,
		httpClient: &http.Client{
			// 2 minutes should be plenty even for long voice memos.
			// Parakeet transcribes ~68 minutes of audio in ~62 seconds
			// on M3, so normal voice memos (under 1 min) are near-instant.
			Timeout: 2 * time.Minute,
		},
	}
}

// transcriptionResponse matches the OpenAI-compatible JSON response
// from parakeet-server: {"text": "the transcribed text"}.
type transcriptionResponse struct {
	Text string `json:"text"`
}

// Transcribe sends audio bytes to the parakeet-server and returns the
// transcribed text. The filename parameter helps the server determine
// the audio format (e.g., "voice.ogg", "memo.wav").
//
// Under the hood, this builds a multipart/form-data request — the same
// format your browser uses when you submit a file upload form. The
// parakeet-server expects this because it mimics the OpenAI Whisper API.
//
// In Python you'd do:
//
//	requests.post(url, files={"file": ("voice.ogg", audio_bytes)})
//
// In Go, we build the multipart body manually with multipart.Writer.
// It's more verbose but does the exact same thing.
func (c *Client) Transcribe(audioBytes []byte, filename string) (string, error) {
	// Build the multipart form body.
	// multipart.Writer handles the boundary markers and content-type
	// headers for each "part" of the form. Think of it as building
	// a POST body that has both a file upload and regular form fields.
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	// Add the audio file as the "file" field.
	// CreateFormFile adds a part with Content-Type: application/octet-stream
	// and Content-Disposition: form-data; name="file"; filename="voice.ogg"
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return "", fmt.Errorf("creating form file: %w", err)
	}
	if _, err := part.Write(audioBytes); err != nil {
		return "", fmt.Errorf("writing audio bytes: %w", err)
	}

	// Add the model field if configured.
	if c.model != "" {
		if err := writer.WriteField("model", c.model); err != nil {
			return "", fmt.Errorf("writing model field: %w", err)
		}
	}

	// Close the writer to finalize the multipart body.
	// This writes the final boundary marker. Forgetting this is a
	// common bug — the server will reject the request as malformed.
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("closing multipart writer: %w", err)
	}

	// Build and send the HTTP request.
	url := c.baseURL + "/audio/transcriptions"
	req, err := http.NewRequest("POST", url, &body)
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}
	// Content-Type must include the boundary string so the server knows
	// where each part starts and ends. writer.FormDataContentType() returns
	// something like "multipart/form-data; boundary=abc123".
	req.Header.Set("Content-Type", writer.FormDataContentType())

	start := time.Now()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("STT request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading STT response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("STT server returned %d: %s", resp.StatusCode, string(respBody))
	}

	var result transcriptionResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return "", fmt.Errorf("parsing STT response: %w", err)
	}

	elapsed := time.Since(start)
	log.Info("transcription complete",
		"duration", elapsed.Round(time.Millisecond),
		"text_len", len(result.Text),
	)

	return strings.TrimSpace(result.Text), nil
}

// IsAvailable checks if the parakeet-server is reachable by hitting
// its health endpoint. Returns false if the server is down — the bot
// can use this to decide whether to attempt transcription or tell the
// user that voice memos aren't available right now.
func (c *Client) IsAvailable() bool {
	// Hit the health endpoint to check if the server is up and ready.
	resp, err := c.httpClient.Get(c.baseURL + "/healthz")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
