// Nova's voice module, wired into her-go's existing TTSEngine / STT client shapes.
//
// Issue #104: Nova v0.4.0 added `voice.Speaker` and `voice.Transcriber`, and its
// OpenRouter adapter replaces two hand-rolled HTTP clients here — one that
// guessed at OpenRouter's TTS response shape (and read a cost header that does
// not exist, so cost was almost always 0) and one that carried both multipart
// and base64-JSON transcription shapes with a string-matching retry rule.
//
// What this file deliberately does NOT do: move Piper or ElevenLabs onto Nova.
// Neither is covered by Nova v0.4.0 (upstream: nova#86), and local Piper is the
// engine this deployment actually uses, so the TTS engine interface stays and
// each engine migrates independently — the migration is reversible per engine.
//
// Ownership: Nova never learns about her-go's config or bot, and her-go's bot
// layer never learns about Nova. The two meet here, in the package that already
// owns speech, and both sides keep their published shapes (`TTSEngine`,
// `SynthesizeResult`, `TranscribeResult`).
package voice

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	novavoice "github.com/AutumnsGrove/nova/voice"
	"github.com/AutumnsGrove/nova/voice/openrouter"

	"her/config"
)

// novaAudioTimeout bounds one synthesis or transcription. A long reply is
// chunked by the caller today (not by this wrapper), and Nova's adapter
// defaults to 120s as well.
const novaAudioTimeout = 120 * time.Second

// newNovaVoiceClient builds Nova's OpenRouter voice client from her-go config.
//
// It returns nil when the API key is missing, because Nova's adapter reports a
// missing key only on the first request — and a TTS engine that fails on every
// call is worse than one the constructor refused to build.
func newNovaVoiceClient(cfg *config.TTSConfig, fallbackAPIKey string, costLookup bool) *openrouter.Client {
	apiKey := cfg.APIKey
	if apiKey == "" {
		apiKey = fallbackAPIKey
	}
	if apiKey == "" {
		log.Warn("TTS enabled with openrouter but no api_key — TTS disabled")
		return nil
	}
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = openrouter.DefaultBaseURL
	}
	return openrouter.New(openrouter.Config{
		APIKey:  apiKey,
		BaseURL: baseURL,
		Referer: "https://github.com/AutumnsGrove/her-go",
		Title:   "her-go",
		// One extra request per synthesis buys the exact cost OpenRouter charged.
		// The hand-rolled client guessed 0 for every raw-bytes response, which
		// made the voice cost invisible in the metrics table.
		CostLookup: costLookup,
	})
}

// NovaTTSClient is a TTS engine backed by Nova's voice module.
//
// It implements the same TTSEngine interface as the Piper and ElevenLabs
// engines, so `TTSClient`, `FallbackTTSClient` and every bot call site are
// unchanged — including cross-engine fallback, which Nova's own role fallback
// cannot express (that stays inside one adapter).
type NovaTTSClient struct {
	speaker   *openrouter.Client
	model     string
	voiceID   string
	replyMode string
}

// NewOpenRouterTTSClient creates the OpenRouter TTS engine on Nova.
//
// The name is kept from the pre-migration client so config wiring, the fallback
// engine table and the bot keep referring to one thing; only the internals moved.
func NewOpenRouterTTSClient(cfg *config.TTSConfig, fallbackAPIKey string) *NovaTTSClient {
	// Exact cost, unless config says otherwise: nil means "on".
	costLookup := cfg.CostLookup == nil || *cfg.CostLookup
	model := cfg.Model
	if model == "" {
		model = openrouter.DefaultSpeechModel
	}
	voiceID := cfg.VoiceID
	if voiceID == "" {
		log.Warn("TTS enabled with openrouter but voice_id is empty — TTS disabled")
		return nil
	}

	speaker := newNovaVoiceClient(cfg, fallbackAPIKey, costLookup)
	if speaker == nil {
		return nil
	}

	replyMode := cfg.ReplyMode
	if replyMode == "" {
		replyMode = "voice"
	}

	log.Info("OpenRouter TTS client initialized (Nova)",
		"engine", "openrouter",
		"model", model,
		"voice", voiceID,
		"reply_mode", replyMode,
		"cost_lookup", costLookup,
	)
	return &NovaTTSClient{speaker: speaker, model: model, voiceID: voiceID, replyMode: replyMode}
}

// Synthesize converts text to OGG/Opus audio for Telegram.
//
// Nova returns the clip exactly as the provider produced it (mp3 or pcm), plus
// the real cost; the Opus conversion for Telegram stays here because it is the
// bot's requirement, not the provider's.
func (c *NovaTTSClient) Synthesize(text string) (SynthesizeResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), novaAudioTimeout)
	defer cancel()

	start := time.Now()
	clip, err := c.speaker.Speak(ctx, novavoice.SpeechRequest{
		Model: c.model,
		Input: text,
		Voice: c.voiceID,
	})
	if err != nil {
		return SynthesizeResult{}, fmt.Errorf("TTS request failed: %w", err)
	}
	if len(clip.Bytes) == 0 {
		return SynthesizeResult{}, fmt.Errorf("TTS returned an empty clip")
	}
	ttsElapsed := time.Since(start)

	oggBytes, err := convertToOpusOGG(clip.Bytes)
	if err != nil {
		return SynthesizeResult{}, err
	}

	log.Info("TTS synthesis complete",
		"engine", "openrouter",
		"model", c.model,
		"tts_duration", ttsElapsed.Round(time.Millisecond),
		"total_duration", time.Since(start).Round(time.Millisecond),
		"text_len", len(text),
		"audio_bytes", len(clip.Bytes),
		"ogg_bytes", len(oggBytes),
		"format", clip.Format,
		"cost", fmt.Sprintf("$%.6f", clip.Usage.CostUSD),
	)
	return SynthesizeResult{Audio: oggBytes, Cost: clip.Usage.CostUSD}, nil
}

// ReplyMode returns whether the bot should always reply with voice ("voice") or
// only when the user sent a voice memo ("match"). App policy, kept in the wrapper.
func (c *NovaTTSClient) ReplyMode() string { return c.replyMode }

// IsAvailable always reports true for a hosted engine: there is no local process
// to health-check, and a paid probe per call would be wasteful. Failures surface
// on the first Synthesize, exactly as they did before.
func (c *NovaTTSClient) IsAvailable() bool { return true }

// ─────────────────────────────────────────────────────────────────────────
// Speech-to-text on Nova
// ─────────────────────────────────────────────────────────────────────────

// transcribeWithNova runs one transcription through Nova's adapter.
//
// Used for remote (OpenRouter) transcription only: the local parakeet sidecar
// stays on the existing multipart path until nova#86 lands an OpenAI-shaped
// adapter that can serve it.
func (c *Client) transcribeWithNova(audioBytes []byte, filename, model string) (TranscribeResult, error) {
	client := openrouter.New(openrouter.Config{
		APIKey:  c.apiKey,
		BaseURL: c.baseURL,
		Referer: "https://github.com/AutumnsGrove/her-go",
		Title:   "her-go",
	})

	format := novaAudioFormat(filename)
	// A zero http.Client.Timeout means "no deadline" to net/http, but
	// context.WithTimeout(0) is an already-expired context. Default instead of
	// inheriting the difference.
	timeout := c.httpClient.Timeout
	if timeout <= 0 {
		timeout = novaAudioTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	start := time.Now()
	transcript, err := client.Transcribe(ctx, novavoice.TranscribeRequest{
		Model:    model,
		Audio:    audioBytes,
		Format:   format,
		Language: "en",
	})
	if err != nil {
		return TranscribeResult{}, fmt.Errorf("STT request failed: %w", err)
	}

	log.Info("STT transcription complete",
		"engine", "openrouter",
		"model", model,
		"format", format,
		"duration", time.Since(start).Round(time.Millisecond),
		"audio_bytes", len(audioBytes),
		"cost", fmt.Sprintf("$%.6f", transcript.Usage.CostUSD),
	)
	return TranscribeResult{Text: transcript.Text, Cost: transcript.Usage.CostUSD}, nil
}

// novaAudioFormat maps a filename to the format Nova requires on a transcription
// request. Nova refuses an empty format on purpose — the container of the bytes
// is a fact the caller has — and Telegram voice notes are always OGG/Opus.
func novaAudioFormat(filename string) novavoice.Format {
	switch strings.ToLower(strings.TrimPrefix(filepath.Ext(filename), ".")) {
	case "ogg", "oga", "opus":
		return novavoice.FormatOGG
	case "wav":
		return novavoice.FormatWAV
	case "mp3":
		return novavoice.FormatMP3
	case "flac":
		return novavoice.FormatFLAC
	case "m4a":
		return novavoice.FormatM4A
	case "webm":
		return novavoice.FormatWebM
	case "aac":
		return novavoice.FormatAAC
	default:
		return novavoice.FormatOGG
	}
}
