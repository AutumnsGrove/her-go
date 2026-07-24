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
	"encoding/base64"
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
// All implementations (Piper, ElevenLabs, OpenRouter) implement this.
type TTSEngine interface {
	Synthesize(text string) (SynthesizeResult, error)
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
//
// fallbackAPIKey is used when cfg.APIKey is empty and the engine is
// remote (openrouter). Pass cfg.OpenRouter.APIKey so remote engines
// can inherit the main API key without duplicating it in config.
func NewTTSClient(cfg *config.TTSConfig, fallbackAPIKey string) *TTSClient {
	if !cfg.Enabled {
		return nil
	}

	var engine TTSEngine

	switch cfg.Engine {
	case config.TTSEnginePiper:
		engine = NewPiperTTSClient(cfg)
	case config.TTSEngineElevenLabs:
		engine = NewElevenLabsTTSClient(cfg)
	case TTSEngineOpenRouter:
		engine = NewOpenRouterTTSClient(cfg, fallbackAPIKey)
	default:
		log.Warn("Unknown TTS engine — TTS disabled", "engine", cfg.Engine)
		return nil
	}

	if engine == nil {
		// Engine constructor returned nil (misconfigured).
		return nil
	}

	if cfg.Fallback != nil {
		if fb := newFallbackTTSEngine(cfg, cfg.Fallback, fallbackAPIKey); fb != nil {
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
func newFallbackTTSEngine(cfg *config.TTSConfig, fb *config.TTSFallbackConfig, fallbackAPIKey string) TTSEngine {
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
	case TTSEngineOpenRouter:
		orCfg := &config.TTSConfig{
			BaseURL:   fb.BaseURL,
			Model:     fb.Model,
			VoiceID:   fb.VoiceID,
			ReplyMode: cfg.ReplyMode,
		}
		return NewOpenRouterTTSClient(orCfg, fallbackAPIKey)
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

func (c *FallbackTTSClient) Synthesize(text string) (SynthesizeResult, error) {
	result, err := c.primary.Synthesize(text)
	if err == nil {
		return result, nil
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
func (c *TTSClient) Synthesize(text string) (SynthesizeResult, error) {
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
// OpenRouter TTS Client (remote engine)
// ─────────────────────────────────────────────────────────────────────────

const (
	// TTSEngineOpenRouter is the config identifier for OpenRouter TTS.
	TTSEngineOpenRouter = "openrouter"
)

// OpenRouterTTSClient talks to OpenRouter's TTS API for text-to-speech.
// Uses the OpenAI-compatible /api/v1/audio/speech endpoint with cost
// tracking via a usage block in the response headers or body.
type OpenRouterTTSClient struct {
	baseURL    string
	apiKey     string
	model      string
	voiceID    string
	replyMode  string
	httpClient *http.Client
}

// NewOpenRouterTTSClient creates a new OpenRouter TTS client from config.
// Returns nil if required fields are missing.
func NewOpenRouterTTSClient(cfg *config.TTSConfig, fallbackAPIKey string) *OpenRouterTTSClient {
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		log.Warn("TTS enabled with openrouter but tts.base_url is empty — TTS disabled")
		return nil
	}

	apiKey := cfg.APIKey
	if apiKey == "" {
		apiKey = fallbackAPIKey
	}
	if apiKey == "" {
		log.Warn("TTS enabled with openrouter but no api_key — TTS disabled")
		return nil
	}

	model := cfg.Model
	if model == "" {
		log.Warn("TTS enabled with openrouter but model is empty — TTS disabled")
		return nil
	}

	voiceID := cfg.VoiceID
	if voiceID == "" {
		log.Warn("TTS enabled with openrouter but voice_id is empty — TTS disabled")
		return nil
	}

	replyMode := cfg.ReplyMode
	if replyMode == "" {
		replyMode = "voice"
	}

	log.Info("OpenRouter TTS client initialized",
		"engine", "openrouter",
		"base_url", baseURL,
		"model", model,
		"voice", voiceID,
		"reply_mode", replyMode,
	)

	return &OpenRouterTTSClient{
		baseURL:   baseURL,
		apiKey:    apiKey,
		model:     model,
		voiceID:   voiceID,
		replyMode: replyMode,
		httpClient: &http.Client{
			Timeout: 60 * time.Second,
		},
	}
}

// openRouterTTSRequest is the JSON body for OpenRouter's /api/v1/audio/speech endpoint.
// Follows the OpenAI TTS API shape.
type openRouterTTSRequest struct {
	Model string `json:"model"`
	Input string `json:"input"`
	Voice string `json:"voice"`
}

// openRouterTTSResponse is the JSON response wrapper when the API returns
// usage info. OpenRouter can return either raw audio bytes or a JSON envelope
// with a `data` field containing base64-encoded audio + usage metadata.
type openRouterTTSResponse struct {
	Data  string `json:"data,omitempty"`  // base64-encoded audio
	Usage *struct {
		Cost float64 `json:"cost"`
	} `json:"usage,omitempty"`
}

// Synthesize converts text to audio bytes via OpenRouter's TTS API.
// Returns OGG/Opus audio suitable for sending as a Telegram voice memo.
//
// OpenRouter's TTS API returns either:
//   - Raw audio bytes (MP3 or PCM) with cost in a response header
//   - JSON with base64-encoded audio + usage.cost field
//
// We handle both formats: check Content-Type to distinguish.
func (c *OpenRouterTTSClient) Synthesize(text string) (SynthesizeResult, error) {
	reqBody := openRouterTTSRequest{
		Model: c.model,
		Input: text,
		Voice: c.voiceID,
	}
	jsonBytes, err := json.Marshal(reqBody)
	if err != nil {
		return SynthesizeResult{}, fmt.Errorf("marshaling TTS request: %w", err)
	}

	url := c.baseURL + "/audio/speech"
	req, err := http.NewRequest("POST", url, bytes.NewReader(jsonBytes))
	if err != nil {
		return SynthesizeResult{}, fmt.Errorf("creating TTS request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.apiKey)

	start := time.Now()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return SynthesizeResult{}, fmt.Errorf("TTS request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return SynthesizeResult{}, fmt.Errorf("OpenRouter TTS returned %d: %s", resp.StatusCode, string(body))
	}

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return SynthesizeResult{}, fmt.Errorf("reading TTS response: %w", err)
	}
	ttsElapsed := time.Since(start)

	var audioBytes []byte
	var cost float64

	// Check Content-Type to determine response format.
	contentType := resp.Header.Get("Content-Type")
	if strings.Contains(contentType, "application/json") {
		// JSON envelope with base64 audio + usage.
		var parsed openRouterTTSResponse
		if err := json.Unmarshal(respBody, &parsed); err != nil {
			return SynthesizeResult{}, fmt.Errorf("parsing JSON TTS response: %w", err)
		}
		if parsed.Data == "" {
			return SynthesizeResult{}, fmt.Errorf("TTS response missing data field")
		}
		audioBytes, err = base64.StdEncoding.DecodeString(parsed.Data)
		if err != nil {
			return SynthesizeResult{}, fmt.Errorf("decoding base64 audio: %w", err)
		}
		if parsed.Usage != nil {
			cost = parsed.Usage.Cost
		}
	} else {
		// Raw audio bytes (MP3 or PCM).
		audioBytes = respBody
		// Cost might be in a header like X-OpenRouter-Cost (check docs).
		// For now, we'll leave it at 0 unless we find the header.
		if costHeader := resp.Header.Get("X-OpenRouter-Cost"); costHeader != "" {
			fmt.Sscanf(costHeader, "%f", &cost)
		}
	}

	// Convert to OGG/Opus for Telegram. ffmpeg auto-detects MP3/PCM input.
	oggBytes, err := convertToOpusOGG(audioBytes)
	if err != nil {
		return SynthesizeResult{}, err
	}

	log.Info("TTS synthesis complete",
		"engine", "openrouter",
		"model", c.model,
		"tts_duration", ttsElapsed.Round(time.Millisecond),
		"total_duration", time.Since(start).Round(time.Millisecond),
		"text_len", len(text),
		"audio_bytes", len(audioBytes),
		"ogg_bytes", len(oggBytes),
		"cost", fmt.Sprintf("$%.6f", cost),
	)

	return SynthesizeResult{Audio: oggBytes, Cost: cost}, nil
}

// ReplyMode returns whether the bot should always reply with voice
// ("voice") or only when the user sends a voice memo ("match").
func (c *OpenRouterTTSClient) ReplyMode() string {
	return c.replyMode
}

// IsAvailable always returns true for the remote OpenRouter engine — there's
// no local process to health-check. Any failure surfaces on the first
// Synthesize call instead (mirrors ElevenLabs and remote STT behavior).
func (c *OpenRouterTTSClient) IsAvailable() bool {
	return true
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
//
// Cost is always zero for local Piper.
func (c *PiperTTSClient) Synthesize(text string) (SynthesizeResult, error) {
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
		return SynthesizeResult{}, fmt.Errorf("marshaling TTS request: %w", err)
	}

	// POST to the speech endpoint.
	url := c.baseURL + "/v1/audio/speech"
	req, err := http.NewRequest("POST", url, bytes.NewReader(jsonBytes))
	if err != nil {
		return SynthesizeResult{}, fmt.Errorf("creating TTS request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return SynthesizeResult{}, fmt.Errorf("TTS request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return SynthesizeResult{}, fmt.Errorf("TTS server returned %d: %s", resp.StatusCode, string(body))
	}

	wavBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return SynthesizeResult{}, fmt.Errorf("reading TTS response: %w", err)
	}

	ttsElapsed := time.Since(start)

	oggBytes, err := convertToOpusOGG(wavBytes)
	if err != nil {
		return SynthesizeResult{}, err
	}

	elapsed := time.Since(start)
	log.Info("TTS synthesis complete",
		"engine", "piper",
		"tts_duration", ttsElapsed.Round(time.Millisecond),
		"total_duration", elapsed.Round(time.Millisecond),
		"text_len", len(text),
		"wav_bytes", len(wavBytes),
		"ogg_bytes", len(oggBytes),
	)

	return SynthesizeResult{Audio: oggBytes, Cost: 0}, nil
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
//
// Cost is always zero — ElevenLabs doesn't expose per-request pricing via
// their API. Track usage via their dashboard instead.
func (c *ElevenLabsTTSClient) Synthesize(text string) (SynthesizeResult, error) {
	reqBody := elevenLabsSpeechRequest{Text: text, ModelID: c.model}
	jsonBytes, err := json.Marshal(reqBody)
	if err != nil {
		return SynthesizeResult{}, fmt.Errorf("marshaling TTS request: %w", err)
	}

	url := "https://api.elevenlabs.io/v1/text-to-speech/" + c.voiceID
	req, err := http.NewRequest("POST", url, bytes.NewReader(jsonBytes))
	if err != nil {
		return SynthesizeResult{}, fmt.Errorf("creating TTS request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("xi-api-key", c.apiKey)

	start := time.Now()
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return SynthesizeResult{}, fmt.Errorf("TTS request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return SynthesizeResult{}, fmt.Errorf("ElevenLabs returned %d: %s", resp.StatusCode, string(body))
	}

	mp3Bytes, err := io.ReadAll(resp.Body)
	if err != nil {
		return SynthesizeResult{}, fmt.Errorf("reading TTS response: %w", err)
	}
	ttsElapsed := time.Since(start)

	// ElevenLabs returns MP3 by default; convertToOpusOGG's ffmpeg call
	// auto-detects the input container, same as it does for Piper's WAV.
	oggBytes, err := convertToOpusOGG(mp3Bytes)
	if err != nil {
		return SynthesizeResult{}, err
	}

	log.Info("TTS synthesis complete",
		"engine", "elevenlabs",
		"tts_duration", ttsElapsed.Round(time.Millisecond),
		"total_duration", time.Since(start).Round(time.Millisecond),
		"text_len", len(text),
		"mp3_bytes", len(mp3Bytes),
		"ogg_bytes", len(oggBytes),
	)

	return SynthesizeResult{Audio: oggBytes, Cost: 0}, nil
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
