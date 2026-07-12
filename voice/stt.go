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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"path/filepath"
	"strings"
	"time"

	"her/config"
	"her/logger"
)

var log = logger.WithPrefix("voice")

// Client is the STT client. It talks to either a local parakeet-server or a
// remote Whisper-compatible API endpoint depending on the configured engine.
// Supports automatic fallback to a different model on timeout/503 errors.
type Client struct {
	engine        string // config.STTEngineParakeet or STTEngineWhisper — determines sidecar and health check behavior
	baseURL       string
	model         string
	apiKey        string // auth token for remote engines; empty for local (parakeet)
	fallbackModel string // fallback model for retries; empty = no fallback
	httpClient    *http.Client
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

	fallbackMsg := "none"
	if cfg.STT.FallbackModel != "" {
		fallbackMsg = cfg.STT.FallbackModel
	}

	log.Info("STT client initialized",
		"engine", cfg.STT.Engine,
		"base_url", baseURL,
		"model", cfg.STT.Model,
		"fallback", fallbackMsg,
	)

	return &Client{
		engine:        cfg.STT.Engine,
		baseURL:       baseURL,
		model:         cfg.STT.Model,
		apiKey:        apiKey,
		fallbackModel: cfg.STT.FallbackModel,
		httpClient: &http.Client{
			// 120 s to handle longer voice memos. OpenRouter's Whisper endpoint
			// scales processing time with audio duration (40s audio can take 60s+
			// to transcribe during high load).
			Timeout: 120 * time.Second,
		},
	}
}

// transcriptionResponse matches the OpenAI Whisper API JSON response.
// OpenRouter extends this with a usage block containing cost info.
type transcriptionResponse struct {
	Text  string `json:"text"`
	Usage *struct {
		Cost float64 `json:"cost"`
	} `json:"usage,omitempty"`
}

// TranscribeResult holds the transcribed text and associated cost.
type TranscribeResult struct {
	Text string
	Cost float64 // USD cost from OpenRouter (0 for local engines)
}

// Transcribe sends audio bytes to the configured STT endpoint and returns the
// transcribed text. The filename parameter signals the audio format to the
// server (e.g. "voice.ogg", "memo.wav").
//
// Two wire formats are supported:
//
//   - **Multipart** (OpenAI Whisper, local parakeet): standard multipart/form-data
//     with a "file" part — the original protocol.
//
//   - **JSON+base64** (OpenRouter): application/json with audio bytes base64-encoded
//     in an `input_audio` object. OpenRouter uses this instead of multipart.
//
// The format is auto-detected from the base URL: OpenRouter gets JSON, everything
// else gets multipart. This keeps config simple — no extra field needed.
//
// If the primary model fails with a retriable error (timeout, 503, 502, 504) and
// a fallback model is configured, automatically retries with the fallback once.
func (c *Client) Transcribe(audioBytes []byte, filename string) (TranscribeResult, error) {
	result, err := c.transcribeWithModel(audioBytes, filename, c.model)

	// If primary failed with a retriable error AND we have a fallback, try it
	if err != nil && c.fallbackModel != "" && isRetriable(err) {
		log.Warn("primary STT failed, trying fallback",
			"primary_model", c.model,
			"fallback_model", c.fallbackModel,
			"error", err.Error(),
		)
		return c.transcribeWithModel(audioBytes, filename, c.fallbackModel)
	}

	return result, err
}

// isRetriable checks whether an error should trigger a fallback retry.
// Retriable: timeouts, 503 (service unavailable), 502 (bad gateway), 504 (gateway timeout).
func isRetriable(err error) bool {
	if err == nil {
		return false
	}
	errStr := err.Error()
	return strings.Contains(errStr, "timeout") ||
		strings.Contains(errStr, "deadline exceeded") ||
		strings.Contains(errStr, "503") ||
		strings.Contains(errStr, "502") ||
		strings.Contains(errStr, "504")
}

// transcribeWithModel performs transcription using the specified model.
// Extracted from Transcribe to enable fallback retries.
func (c *Client) transcribeWithModel(audioBytes []byte, filename string, model string) (TranscribeResult, error) {
	if isOpenRouter(c.baseURL) {
		return c.transcribeJSON(audioBytes, filename, model)
	}
	return c.transcribeMultipart(audioBytes, filename, model)
}

// isOpenRouter checks whether the base URL points to OpenRouter's API.
func isOpenRouter(baseURL string) bool {
	return strings.Contains(baseURL, "openrouter.ai")
}

// audioFormatFromFilename extracts the format string OpenRouter expects
// from the filename extension. Falls back to "ogg" if unrecognized.
func audioFormatFromFilename(filename string) string {
	ext := strings.TrimPrefix(filepath.Ext(filename), ".")
	switch ext {
	case "ogg", "wav", "mp3", "flac", "webm":
		return ext
	default:
		return "ogg"
	}
}

// transcribeJSON uses OpenRouter's JSON+base64 format.
func (c *Client) transcribeJSON(audioBytes []byte, filename string, model string) (TranscribeResult, error) {
	payload := map[string]any{
		"model": model,
		"input_audio": map[string]string{
			"data":   base64.StdEncoding.EncodeToString(audioBytes),
			"format": audioFormatFromFilename(filename),
		},
		"language": "en",
	}

	body, err := json.Marshal(payload)
	if err != nil {
		return TranscribeResult{}, fmt.Errorf("marshaling JSON: %w", err)
	}

	url := c.baseURL + "/audio/transcriptions"
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return TranscribeResult{}, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	return c.doTranscribe(req)
}

// transcribeMultipart uses the standard OpenAI Whisper multipart/form-data format.
func (c *Client) transcribeMultipart(audioBytes []byte, filename string, model string) (TranscribeResult, error) {
	var body bytes.Buffer
	writer := multipart.NewWriter(&body)

	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return TranscribeResult{}, fmt.Errorf("creating form file: %w", err)
	}
	if _, err := part.Write(audioBytes); err != nil {
		return TranscribeResult{}, fmt.Errorf("writing audio bytes: %w", err)
	}

	if model != "" {
		if err := writer.WriteField("model", model); err != nil {
			return TranscribeResult{}, fmt.Errorf("writing model field: %w", err)
		}
	}

	if err := writer.Close(); err != nil {
		return TranscribeResult{}, fmt.Errorf("closing multipart writer: %w", err)
	}

	url := c.baseURL + "/audio/transcriptions"
	req, err := http.NewRequest("POST", url, &body)
	if err != nil {
		return TranscribeResult{}, fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	return c.doTranscribe(req)
}

// doTranscribe executes the HTTP request and parses the transcription response.
// Shared by both JSON and multipart paths.
func (c *Client) doTranscribe(req *http.Request) (TranscribeResult, error) {
	start := time.Now()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return TranscribeResult{}, fmt.Errorf("STT request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return TranscribeResult{}, fmt.Errorf("reading STT response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return TranscribeResult{}, fmt.Errorf("STT server returned %d: %s", resp.StatusCode, string(respBody))
	}

	var parsed transcriptionResponse
	if err := json.Unmarshal(respBody, &parsed); err != nil {
		return TranscribeResult{}, fmt.Errorf("parsing STT response: %w", err)
	}

	elapsed := time.Since(start)

	trimmed := strings.TrimSpace(parsed.Text)
	if trimmed == "" {
		return TranscribeResult{}, fmt.Errorf("transcription returned empty text (audio may be silent or too short)")
	}

	var cost float64
	if parsed.Usage != nil {
		cost = parsed.Usage.Cost
	}

	log.Info("transcription complete",
		"duration", elapsed.Round(time.Millisecond),
		"text_len", len(trimmed),
		"cost", fmt.Sprintf("$%.6f", cost),
	)

	return TranscribeResult{Text: trimmed, Cost: cost}, nil
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
