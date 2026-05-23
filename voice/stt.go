// Package voice handles speech-to-text (v0.3) and text-to-speech (v0.5).
//
// STT supports two engines:
//
//   - "parakeet": a Python sidecar (parakeet-mlx-fastapi) running locally.
//     The bot spawns it automatically. Apple Silicon only (MLX framework).
//
//   - "whisper": any OpenAI-compatible remote endpoint (OpenRouter, OpenAI,
//     etc.) using the standard /v1/audio/transcriptions multipart API.
//     Works on any platform — good choice for VPS deployment.
//
// Both engines speak the same OpenAI-compatible multipart/form-data protocol,
// so the wire format is identical. The only difference is the base URL and
// whether an Authorization header is needed.
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

// Client is the STT client. It talks to either a local parakeet-server or a
// remote Whisper-compatible API endpoint depending on the configured engine.
type Client struct {
	engine     string // config.STTEngineParakeet or STTEngineWhisper — determines sidecar and health check behavior
	baseURL    string
	model      string
	apiKey     string // auth token for remote engines; empty for local (parakeet)
	httpClient *http.Client
}

// NewClient creates an STT client from config. Returns nil if voice is
// disabled or the base URL is empty — callers should nil-check before use.
//
// fallbackAPIKey is used when cfg.STT.APIKey is empty and the engine is
// remote (whisper). Pass cfg.OpenRouter.APIKey so the caller doesn't need
// to mutate the config struct.
//
// In Go, returning nil from a constructor is a common pattern to signal
// "this feature is disabled". It's explicit and forces the caller to check,
// unlike Python where you might return a no-op object.
func NewClient(cfg *config.VoiceConfig, fallbackAPIKey string) *Client {
	if !cfg.Enabled {
		return nil
	}

	baseURL := strings.TrimRight(cfg.STT.BaseURL, "/")
	if baseURL == "" {
		log.Warn("voice enabled but stt.base_url is empty — STT disabled")
		return nil
	}

	apiKey := cfg.STT.APIKey
	if apiKey == "" && cfg.STT.Engine == config.STTEngineWhisper {
		apiKey = fallbackAPIKey
	}

	log.Info("STT client initialized",
		"engine", cfg.STT.Engine,
		"base_url", baseURL,
		"model", cfg.STT.Model,
	)

	return &Client{
		engine:  cfg.STT.Engine,
		baseURL: baseURL,
		model:   cfg.STT.Model,
		apiKey:  apiKey,
		httpClient: &http.Client{
			// 30 s is generous for normal voice memos. Remote Whisper APIs
			// typically respond in 2-5 s for under-a-minute clips.
			Timeout: 30 * time.Second,
		},
	}
}

// transcriptionResponse matches the OpenAI Whisper API JSON response.
type transcriptionResponse struct {
	Text string `json:"text"`
}

// Transcribe sends audio bytes to the configured STT endpoint and returns the
// transcribed text. The filename parameter signals the audio format to the
// server (e.g. "voice.ogg", "memo.wav").
//
// The wire format is identical for both engines: multipart/form-data with a
// "file" part and an optional "model" field — same as the OpenAI Whisper API.
// The only engine-specific difference is the Authorization header.
//
// Python equivalent:
//
//	requests.post(url, files={"file": ("voice.ogg", audio_bytes)},
//	              headers={"Authorization": f"Bearer {api_key}"})
func (c *Client) Transcribe(audioBytes []byte, filename string) (string, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return "", fmt.Errorf("creating form file: %w", err)
	}
	if _, err := part.Write(audioBytes); err != nil {
		return "", fmt.Errorf("writing audio bytes: %w", err)
	}

	if c.model != "" {
		if err := writer.WriteField("model", c.model); err != nil {
			return "", fmt.Errorf("writing model field: %w", err)
		}
	}

	// Close must be called before reading from body — it writes the final
	// boundary marker. Missing this produces a malformed request.
	if err := writer.Close(); err != nil {
		return "", fmt.Errorf("closing multipart writer: %w", err)
	}

	url := c.baseURL + "/audio/transcriptions"
	req, err := http.NewRequest("POST", url, &body)
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	// Remote engines (whisper) require Bearer auth. Local engines (parakeet)
	// run on localhost with no auth — apiKey will be empty.
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

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

	trimmed := strings.TrimSpace(result.Text)
	if trimmed == "" {
		return "", fmt.Errorf("transcription returned empty text (audio may be silent or too short)")
	}

	log.Info("transcription complete",
		"duration", elapsed.Round(time.Millisecond),
		"text_len", len(trimmed),
	)

	return trimmed, nil
}

// IsAvailable checks whether the STT backend is reachable.
//
// For "parakeet" (local sidecar), hits the /healthz endpoint the sidecar
// exposes. For "whisper" and other remote engines, returns true immediately —
// the remote API is presumed up, and any failure surfaces on the first
// Transcribe call with a clear error.
func (c *Client) IsAvailable() bool {
	if c.engine != config.STTEngineParakeet {
		// Remote engine — no local process to health-check.
		return true
	}
	resp, err := c.httpClient.Get(c.baseURL + "/healthz")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
