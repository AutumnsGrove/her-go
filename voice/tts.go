// tts.go — Text-to-speech client for the Piper TTS sidecar.
//
// The Piper sidecar (scripts/tts_server.py) exposes an OpenAI-compatible
// /v1/audio/speech endpoint. We POST JSON with the text and voice settings,
// and get back WAV audio bytes which we convert to OGG/Opus for Telegram.
//
// This is the mirror image of stt.go:
//
//	STT: audio bytes in  → text out
//	TTS: text in          → audio bytes out
package voice

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"strings"
	"time"

	"her/config"
)

// TTSClient talks to the local Piper TTS sidecar for text-to-speech.
type TTSClient struct {
	baseURL    string
	model      string
	voiceID    string
	speed      float64
	replyMode  string
	httpClient *http.Client
}

// NewTTSClient creates a new TTS client from config. Returns nil if
// TTS is disabled or misconfigured — callers should nil-check.
func NewTTSClient(cfg *config.TTSConfig) *TTSClient {
	if !cfg.Enabled {
		return nil
	}

	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		log.Warn("TTS enabled but tts.base_url is empty — TTS disabled")
		return nil
	}

	voiceID := cfg.VoiceID
	if voiceID == "" {
		voiceID = "en_GB-southern_english_female-low"
	}

	speed := cfg.Speed
	if speed <= 0 {
		speed = 1.0
	}

	replyMode := cfg.ReplyMode
	if replyMode == "" {
		replyMode = "voice"
	}

	log.Info("TTS client initialized",
		"engine", cfg.Engine,
		"base_url", baseURL,
		"model", cfg.Model,
		"voice", voiceID,
		"speed", speed,
		"reply_mode", replyMode,
	)

	return &TTSClient{
		baseURL:   baseURL,
		model:     cfg.Model,
		voiceID:   voiceID,
		speed:     speed,
		replyMode: replyMode,
		httpClient: &http.Client{
			// TTS for a typical chat message (1-3 sentences) should be
			// well under 30 seconds. Longer timeout as a safety net.
			Timeout: 1 * time.Minute,
		},
	}
}

// speechRequest is the JSON body for the OpenAI-compatible /v1/audio/speech
// endpoint. Same shape as the OpenAI TTS API.
type speechRequest struct {
	Model          string  `json:"model"`
	Input          string  `json:"input"`
	Voice          string  `json:"voice"`
	Speed          float64 `json:"speed,omitempty"`
	ResponseFormat string  `json:"response_format"`
}

// Synthesize converts text to audio bytes. Returns OGG/Opus audio
// suitable for sending as a Telegram voice memo.
//
// The flow:
//  1. POST JSON to /v1/audio/speech requesting WAV format
//  2. Get back raw WAV bytes
//  3. Convert WAV → OGG/Opus via ffmpeg (Telegram requires Opus for voice memos)
//
// We request WAV from the server (no ffmpeg dependency on the Python side)
// and convert on the Go side where we already require ffmpeg for STT anyway.
func (c *TTSClient) Synthesize(text string) ([]byte, error) {
	// Build the request body.
	reqBody := speechRequest{
		Model:          c.model,
		Input:          text,
		Voice:          c.voiceID,
		Speed:          c.speed,
		ResponseFormat: "wav",
	}

	jsonBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshaling TTS request: %w", err)
	}

	// POST to the speech endpoint.
	url := c.baseURL + "/v1/audio/speech"
	req, err := http.NewRequest("POST", url, bytes.NewReader(jsonBytes))
	if err != nil {
		return nil, fmt.Errorf("creating TTS request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("TTS request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("TTS server returned %d: %s", resp.StatusCode, string(body))
	}

	wavBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading TTS response: %w", err)
	}

	ttsElapsed := time.Since(start)

	// Convert WAV → OGG/Opus for Telegram.
	// Telegram voice memos MUST be Opus-encoded in an OGG container.
	// ffmpeg does this conversion: pipe WAV in via stdin, get OGG out
	// via stdout. The -c:a libopus flag selects the Opus codec.
	//
	// The -ar 48000 is critical: Opus internally operates at 48kHz,
	// and Piper's "low" voice models output at 16kHz. Without an
	// explicit resample, the OGG container can get ambiguous sample
	// rate metadata — Telegram then plays the audio at 3x speed
	// (chipmunk effect). Forcing 48kHz ensures correct playback.
	cmd := exec.Command("ffmpeg",
		"-i", "pipe:0", // read WAV from stdin
		"-ar", "48000", // resample to 48kHz (Opus native rate)
		"-c:a", "libopus", // encode with Opus codec
		"-b:a", "64k", // 64kbps — good quality for speech
		"-application", "voip", // optimize for speech (vs music)
		"-f", "ogg", // output format
		"pipe:1", // write to stdout
	)
	cmd.Stdin = bytes.NewReader(wavBytes)

	var oggBuf bytes.Buffer
	var errBuf bytes.Buffer
	cmd.Stdout = &oggBuf
	cmd.Stderr = &errBuf

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg WAV→OGG conversion failed: %w (stderr: %s)", err, errBuf.String())
	}

	elapsed := time.Since(start)
	log.Info("TTS synthesis complete",
		"tts_duration", ttsElapsed.Round(time.Millisecond),
		"total_duration", elapsed.Round(time.Millisecond),
		"text_len", len(text),
		"wav_bytes", len(wavBytes),
		"ogg_bytes", oggBuf.Len(),
	)

	return oggBuf.Bytes(), nil
}

// ReplyMode returns whether the bot should always reply with voice
// ("voice") or only when the user sends a voice memo ("match").
func (c *TTSClient) ReplyMode() string {
	return c.replyMode
}

// IsAvailable checks if the Piper TTS sidecar is reachable.
func (c *TTSClient) IsAvailable() bool {
	resp, err := c.httpClient.Get(c.baseURL + "/healthz")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
