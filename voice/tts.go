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

// TTSEngine is the common interface for all TTS engines.
// Both PiperTTSClient and ElevenLabsTTSClient implement this.
type TTSEngine interface {
	Synthesize(text string) ([]byte, error)
	ReplyMode() string
	IsAvailable() bool
}

// TTSClient wraps a TTSEngine implementation (Piper or Cloudflare).
// The bot code interacts with this wrapper, which dispatches to the
// configured engine under the hood.
type TTSClient struct {
	engine TTSEngine
}

// NewTTSClient creates a new TTS client from config, dispatching to
// the appropriate engine based on cfg.Engine. Returns nil if TTS is
// disabled or misconfigured — callers should nil-check.
func NewTTSClient(cfg *config.TTSConfig) *TTSClient {
	if !cfg.Enabled {
		return nil
	}

	var engine TTSEngine

	switch cfg.Engine {
	case config.TTSEnginePiper:
		engine = NewPiperTTSClient(cfg)
	case config.TTSEngineElevenLabs:
		engine = NewElevenLabsTTSClient(cfg)
	default:
		log.Warn("Unknown TTS engine — TTS disabled", "engine", cfg.Engine)
		return nil
	}

	if engine == nil {
		// Engine constructor returned nil (misconfigured).
		return nil
	}

	if cfg.Fallback != nil {
		if fb := newFallbackTTSEngine(cfg, cfg.Fallback); fb != nil {
			engine = &FallbackTTSClient{primary: engine, fallback: fb}
		} else {
			log.Warn("tts.fallback configured but failed to initialize — continuing without fallback")
		}
	}

	return &TTSClient{engine: engine}
}

// newFallbackTTSEngine builds the fallback engine from a TTSFallbackConfig.
// Reuses NewPiperTTSClient by assembling a throwaway TTSConfig from the
// fallback fields plus the primary config's shared settings (reply mode,
// pause timings) — those aren't engine-specific, so there's no reason to
// duplicate them in TTSFallbackConfig.
func newFallbackTTSEngine(cfg *config.TTSConfig, fb *config.TTSFallbackConfig) TTSEngine {
	switch fb.Engine {
	case config.TTSEnginePiper, "":
		piperCfg := &config.TTSConfig{
			BaseURL:   fb.BaseURL,
			Model:     fb.Model,
			VoiceID:   fb.VoiceID,
			ReplyMode: cfg.ReplyMode,
			Pauses:    cfg.Pauses,
		}
		return NewPiperTTSClient(piperCfg)
	default:
		log.Warn("Unknown TTS fallback engine", "engine", fb.Engine)
		return nil
	}
}

// FallbackTTSClient wraps a primary TTSEngine with an automatic fallback.
// If the primary fails for any reason — API error, network failure, or
// quota exhausted (e.g. hitting ElevenLabs' free-tier character limit
// mid-month) — it retries once with the fallback engine so voice replies
// keep working instead of silently failing.
type FallbackTTSClient struct {
	primary  TTSEngine
	fallback TTSEngine
}

func (c *FallbackTTSClient) Synthesize(text string) ([]byte, error) {
	audio, err := c.primary.Synthesize(text)
	if err == nil {
		return audio, nil
	}
	log.Warn("primary TTS engine failed, falling back", "error", err.Error())
	return c.fallback.Synthesize(text)
}

// ReplyMode uses the primary engine's setting — fallback is purely a
// synthesis-time failover, not a separate reply-mode configuration.
func (c *FallbackTTSClient) ReplyMode() string {
	return c.primary.ReplyMode()
}

// IsAvailable reports true if either the primary or fallback engine is up,
// since a Synthesize call would still succeed via failover either way.
func (c *FallbackTTSClient) IsAvailable() bool {
	return c.primary.IsAvailable() || c.fallback.IsAvailable()
}

// Synthesize delegates to the underlying engine.
func (c *TTSClient) Synthesize(text string) ([]byte, error) {
	return c.engine.Synthesize(text)
}

// ReplyMode delegates to the underlying engine.
func (c *TTSClient) ReplyMode() string {
	return c.engine.ReplyMode()
}

// IsAvailable delegates to the underlying engine.
func (c *TTSClient) IsAvailable() bool {
	return c.engine.IsAvailable()
}

// ─────────────────────────────────────────────────────────────────────────
// Piper TTS Client (local engine)
// ─────────────────────────────────────────────────────────────────────────

// PiperTTSClient talks to the local Piper TTS sidecar for text-to-speech.
type PiperTTSClient struct {
	baseURL    string
	model      string
	voiceID    string
	speed      float64
	replyMode  string
	pauses     *config.TTSPauseConfig
	httpClient *http.Client
}

// NewPiperTTSClient creates a new Piper TTS client from config. Returns nil
// if required fields are missing.
func NewPiperTTSClient(cfg *config.TTSConfig) *PiperTTSClient {
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		log.Warn("TTS enabled with piper but tts.base_url is empty — TTS disabled")
		return nil
	}

	voiceID := cfg.VoiceID
	if voiceID == "" {
		log.Warn("TTS enabled with piper but voice_id is empty — TTS disabled")
		return nil
	}

	model := cfg.Model
	if model == "" {
		log.Warn("TTS enabled with piper but model is empty — TTS disabled")
		return nil
	}

	speed := cfg.Speed
	if speed <= 0 {
		speed = 1.0
	}

	replyMode := cfg.ReplyMode
	if replyMode == "" {
		replyMode = "voice"
	}

	log.Info("Piper TTS client initialized",
		"engine", "piper",
		"base_url", baseURL,
		"model", model,
		"voice", voiceID,
		"speed", speed,
		"reply_mode", replyMode,
	)

	return &PiperTTSClient{
		baseURL:   baseURL,
		model:     model,
		voiceID:   voiceID,
		speed:     speed,
		replyMode: replyMode,
		pauses:    &cfg.Pauses,
		httpClient: &http.Client{
			// TTS for a typical chat message (1-3 sentences) should be
			// well under 30 seconds. Longer timeout as a safety net.
			Timeout: 1 * time.Minute,
		},
	}
}

// speechRequest is the JSON body for the /v1/audio/speech endpoint.
// Extends the OpenAI TTS shape with a pauses field for hot-reloadable
// punctuation pause durations.
type speechRequest struct {
	Model          string        `json:"model"`
	Input          string        `json:"input"`
	Voice          string        `json:"voice"`
	Speed          float64       `json:"speed,omitempty"`
	ResponseFormat string        `json:"response_format"`
	Pauses         *speechPauses `json:"pauses,omitempty"`
}

// speechPauses carries per-request pause overrides to the TTS sidecar.
// Values come from config.yaml and are sent on every request so the
// sidecar picks up config changes without a restart.
type speechPauses struct {
	ParagraphMS int `json:"paragraph_ms,omitempty"`
	LineMS      int `json:"line_ms,omitempty"`
	SentenceMS  int `json:"sentence_ms,omitempty"`
	CommaMS     int `json:"comma_ms,omitempty"`
	SemiMS      int `json:"semi_ms,omitempty"`
}

// Synthesize converts text to audio bytes using the Piper TTS sidecar.
// Returns OGG/Opus audio suitable for sending as a Telegram voice memo.
//
// The flow:
//  1. POST JSON to /v1/audio/speech requesting WAV format
//  2. Get back raw WAV bytes
//  3. Convert WAV → OGG/Opus via ffmpeg (Telegram requires Opus for voice memos)
//
// We request WAV from the server (no ffmpeg dependency on the Python side)
// and convert on the Go side where we already require ffmpeg for STT anyway.
func (c *PiperTTSClient) Synthesize(text string) ([]byte, error) {
	// Build the request body.
	reqBody := speechRequest{
		Model:          c.model,
		Input:          text,
		Voice:          c.voiceID,
		Speed:          c.speed,
		ResponseFormat: "wav",
	}
	if c.pauses != nil {
		reqBody.Pauses = &speechPauses{
			ParagraphMS: c.pauses.Paragraph,
			LineMS:      c.pauses.Line,
			SentenceMS:  c.pauses.Sentence,
			CommaMS:     c.pauses.Comma,
			SemiMS:      c.pauses.Semi,
		}
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

	oggBytes, err := convertToOpusOGG(wavBytes)
	if err != nil {
		return nil, err
	}

	elapsed := time.Since(start)
	log.Info("TTS synthesis complete",
		"tts_duration", ttsElapsed.Round(time.Millisecond),
		"total_duration", elapsed.Round(time.Millisecond),
		"text_len", len(text),
		"wav_bytes", len(wavBytes),
		"ogg_bytes", len(oggBytes),
	)

	return oggBytes, nil
}

// convertToOpusOGG converts audio bytes to OGG/Opus, which Telegram requires
// for voice memos. ffmpeg auto-detects the input container (WAV from Piper,
// MP3 from ElevenLabs) by probing the stream, so no input format flag is needed.
//
// The -ar 48000 is critical: Opus internally operates at 48kHz, and source
// audio (e.g. Piper's "low" voice models) can be as low as 16kHz. Without an
// explicit resample, the OGG container can get ambiguous sample rate
// metadata — Telegram then plays the audio at 3x speed (chipmunk effect).
// Forcing 48kHz ensures correct playback.
func convertToOpusOGG(audioBytes []byte) ([]byte, error) {
	cmd := exec.Command("ffmpeg",
		"-i", "pipe:0", // read audio from stdin
		"-ar", "48000", // resample to 48kHz (Opus native rate)
		"-c:a", "libopus", // encode with Opus codec
		"-b:a", "64k", // 64kbps — good quality for speech
		"-application", "voip", // optimize for speech (vs music)
		"-f", "ogg", // output format
		"pipe:1", // write to stdout
	)
	cmd.Stdin = bytes.NewReader(audioBytes)

	var oggBuf bytes.Buffer
	var errBuf bytes.Buffer
	cmd.Stdout = &oggBuf
	cmd.Stderr = &errBuf

	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("ffmpeg audio→OGG conversion failed: %w (stderr: %s)", err, errBuf.String())
	}

	return oggBuf.Bytes(), nil
}

// ReplyMode returns whether the bot should always reply with voice
// ("voice") or only when the user sends a voice memo ("match").
func (c *PiperTTSClient) ReplyMode() string {
	return c.replyMode
}

// IsAvailable checks if the Piper TTS sidecar is reachable.
func (c *PiperTTSClient) IsAvailable() bool {
	resp, err := c.httpClient.Get(c.baseURL + "/healthz")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// ─────────────────────────────────────────────────────────────────────────
// ElevenLabs TTS Client (remote engine)
// ─────────────────────────────────────────────────────────────────────────

// elevenLabsDefaultModel is used when tts.model is left empty in config.yaml.
// eleven_flash_v2_5 is ElevenLabs' lowest-latency model — a good default for
// chat replies where response time matters more than maximum fidelity.
const elevenLabsDefaultModel = "eleven_flash_v2_5"

// ElevenLabsTTSClient talks to the ElevenLabs text-to-speech API. Unlike
// Piper, there's no local process — every Synthesize call is a network
// request, and there's no local RAM cost.
type ElevenLabsTTSClient struct {
	apiKey     string
	voiceID    string
	model      string
	replyMode  string
	httpClient *http.Client
}

// NewElevenLabsTTSClient creates a new ElevenLabs TTS client from config.
// Returns nil if required fields are missing.
func NewElevenLabsTTSClient(cfg *config.TTSConfig) *ElevenLabsTTSClient {
	if cfg.APIKey == "" {
		log.Warn("TTS enabled with elevenlabs but tts.api_key is empty — TTS disabled")
		return nil
	}

	voiceID := cfg.VoiceID
	if voiceID == "" {
		log.Warn("TTS enabled with elevenlabs but voice_id is empty — TTS disabled")
		return nil
	}

	model := cfg.Model
	if model == "" {
		model = elevenLabsDefaultModel
	}

	replyMode := cfg.ReplyMode
	if replyMode == "" {
		replyMode = "voice"
	}

	log.Info("ElevenLabs TTS client initialized",
		"engine", "elevenlabs",
		"voice_id", voiceID,
		"model", model,
		"reply_mode", replyMode,
	)

	return &ElevenLabsTTSClient{
		apiKey:    cfg.APIKey,
		voiceID:   voiceID,
		model:     model,
		replyMode: replyMode,
		httpClient: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// elevenLabsSpeechRequest is the JSON body for ElevenLabs' text-to-speech endpoint.
type elevenLabsSpeechRequest struct {
	Text    string `json:"text"`
	ModelID string `json:"model_id"`
}

// Synthesize converts text to audio bytes via the ElevenLabs API. Returns
// OGG/Opus audio suitable for sending as a Telegram voice memo.
func (c *ElevenLabsTTSClient) Synthesize(text string) ([]byte, error) {
	reqBody := elevenLabsSpeechRequest{Text: text, ModelID: c.model}
	jsonBytes, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("marshaling TTS request: %w", err)
	}

	url := "https://api.elevenlabs.io/v1/text-to-speech/" + c.voiceID
	req, err := http.NewRequest("POST", url, bytes.NewReader(jsonBytes))
	if err != nil {
		return nil, fmt.Errorf("creating TTS request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("xi-api-key", c.apiKey)

	start := time.Now()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("TTS request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("ElevenLabs returned %d: %s", resp.StatusCode, string(body))
	}

	mp3Bytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading TTS response: %w", err)
	}
	ttsElapsed := time.Since(start)

	// ElevenLabs returns MP3 by default; convertToOpusOGG's ffmpeg call
	// auto-detects the input container, same as it does for Piper's WAV.
	oggBytes, err := convertToOpusOGG(mp3Bytes)
	if err != nil {
		return nil, err
	}

	log.Info("TTS synthesis complete",
		"engine", "elevenlabs",
		"tts_duration", ttsElapsed.Round(time.Millisecond),
		"total_duration", time.Since(start).Round(time.Millisecond),
		"text_len", len(text),
		"mp3_bytes", len(mp3Bytes),
		"ogg_bytes", len(oggBytes),
	)

	return oggBytes, nil
}

// ReplyMode returns whether the bot should always reply with voice
// ("voice") or only when the user sends a voice memo ("match").
func (c *ElevenLabsTTSClient) ReplyMode() string {
	return c.replyMode
}

// IsAvailable always returns true for the remote ElevenLabs engine — there's
// no local process to health-check. Any failure surfaces on the first
// Synthesize call instead (mirrors voice.Client.IsAvailable for STT's
// remote "whisper" engine).
func (c *ElevenLabsTTSClient) IsAvailable() bool {
	return true
}
