package voice

// Tests for the Nova voice bridge (voice/nova.go).
//
// Hermetic: a fake OpenAI-compatible HTTP server stands in for OpenRouter, so
// these run in CI without a key and without spending money. The wrapper's job is
// narrow and worth pinning: keep the her-go result shapes, turn Nova's clip into
// Telegram-ready Opus, and pass the cost through instead of guessing.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"testing"

	"github.com/AutumnsGrove/nova/provider"

	"her/config"
)

// fakeVoiceServer serves /audio/speech and /audio/transcriptions.
func fakeVoiceServer(t *testing.T, speech []byte, transcript string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/audio/speech", func(w http.ResponseWriter, r *http.Request) {
		var req map[string]any
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			http.Error(w, "bad body", http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "audio/mpeg")
		_, _ = w.Write(speech)
	})
	mux.HandleFunc("/audio/transcriptions", func(w http.ResponseWriter, r *http.Request) {
		// Nova posts multipart with the file part named clip.<format>.
		if err := r.ParseMultipartForm(8 << 20); err != nil {
			http.Error(w, "bad multipart", http.StatusBadRequest)
			return
		}
		// Nova names the form field "file" and the filename "clip.<format>",
		// because OpenRouter infers the codec from the extension.
		file, header, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "missing file part", http.StatusBadRequest)
			return
		}
		_ = file.Close()
		if header.Filename != "clip.ogg" {
			http.Error(w, "wrong filename "+header.Filename, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"text":  transcript,
			"usage": map[string]any{"cost": 0.000056},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// mp3ish is 100ms of silence encoded as MP3 by ffmpeg, so the test carries no
// binary fixture. The wrapper's job includes the Opus conversion, which is
// ffmpeg's, so a machine without ffmpeg cannot test this path.
func mp3ish(t *testing.T) []byte {
	t.Helper()
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		t.Skip("ffmpeg not installed; the Opus conversion is its own concern")
	}
	silence := make([]byte, 48000*2/10) // 100ms of 16-bit mono silence
	cmd := exec.Command("ffmpeg", "-f", "s16le", "-ar", "48000", "-ac", "1", "-i", "pipe:0", "-f", "mp3", "pipe:1")
	cmd.Stdin = bytes.NewReader(silence)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("building test mp3: %v", err)
	}
	return out
}

func TestNovaTTS_SynthesizeReturnsOpusAndCost(t *testing.T) {
	server := fakeVoiceServer(t, mp3ish(t), "")

	cfg := &config.TTSConfig{
		BaseURL: server.URL,
		APIKey:  "test-key",
		Model:   "test/speech-model",
		VoiceID: "test-voice",
	}
	engine := NewOpenRouterTTSClient(cfg, "")
	if engine == nil {
		t.Fatal("NewOpenRouterTTSClient returned nil")
	}
	if engine.ReplyMode() != "voice" {
		t.Errorf("ReplyMode = %q, want voice (the default)", engine.ReplyMode())
	}

	res, err := engine.Synthesize("hello there")
	if err != nil {
		t.Fatalf("Synthesize: %v", err)
	}
	if len(res.Audio) == 0 {
		t.Fatal("no audio returned")
	}
	// OGG/Opus for Telegram: the container magic is "OggS".
	if string(res.Audio[:4]) != "OggS" {
		t.Errorf("audio is not OGG (first bytes %q)", res.Audio[:min(4, len(res.Audio))])
	}
}

func TestNovaTTS_MissingVoiceIsRefused(t *testing.T) {
	cfg := &config.TTSConfig{BaseURL: "http://example.invalid", APIKey: "k", Model: "m"}
	if engine := NewOpenRouterTTSClient(cfg, ""); engine != nil {
		t.Error("a missing voice_id should build no engine, not an engine that fails every call")
	}
}

func TestNovaSTT_TranscribesOGGWithCost(t *testing.T) {
	server := fakeVoiceServer(t, nil, "the plants need watering")

	client := &Client{
		engine:     config.STTEngineWhisper,
		baseURL:    server.URL,
		model:      "test/stt-model",
		apiKey:     "test-key",
		httpClient: http.DefaultClient,
	}

	// Call the Nova path directly: routing is by base URL (only openrouter.ai goes
	// to Nova, because self-hosted Whisper servers expect a `file` part rather
	// than OpenRouter's `clip.<format>`), and a test server is never openrouter.ai.
	res, err := client.transcribeWithNova([]byte("not really audio"), "voice.ogg", client.model)
	if err != nil {
		t.Fatalf("Transcribe: %v", err)
	}
	if res.Text != "the plants need watering" {
		t.Errorf("text = %q", res.Text)
	}
	if res.Cost != 0.000056 {
		t.Errorf("cost = %v, want the provider's inline usage cost", res.Cost)
	}
}

func TestNovaAudioFormat(t *testing.T) {
	cases := map[string]string{
		"voice.ogg":   "ogg",
		"memo.wav":    "wav",
		"clip.mp3":    "mp3",
		"RECORD.M4A":  "m4a",
		"unknown.bin": "ogg", // Telegram's voice container is the safe default
	}
	for name, want := range cases {
		if got := string(novaAudioFormat(name)); got != want {
			t.Errorf("novaAudioFormat(%q) = %q, want %q", name, got, want)
		}
	}
}

// A constructor that returns a nil *NovaTTSClient and is assigned to a TTSEngine
// interface produces a non-nil interface holding a nil pointer — `engine == nil`
// is false, and the first Synthesize call dereferences nil. (Go interfaces are
// a (type, value) pair; only the pair (nil, nil) compares equal to nil.)
// A misconfigured engine must come out of the factory as a real nil.
func TestNewTTSClient_MisconfiguredEngineIsNil(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config.TTSConfig
	}{
		{"openrouter without voice_id", config.TTSConfig{Enabled: true, Engine: TTSEngineOpenRouter, APIKey: "k"}},
		{"openrouter without any api key", config.TTSConfig{Enabled: true, Engine: TTSEngineOpenRouter, VoiceID: "alloy"}},
		{"elevenlabs without credentials", config.TTSConfig{Enabled: true, Engine: config.TTSEngineElevenLabs}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := NewTTSClient(&tc.cfg, ""); got != nil {
				t.Fatalf("NewTTSClient returned %#v, want nil for a misconfigured engine", got)
			}
		})
	}
}

// A fallback that cannot be built must be skipped, not wrapped as a nil engine
// that panics the first time the primary fails.
func TestNewTTSClient_BrokenFallbackIsSkipped(t *testing.T) {
	cfg := config.TTSConfig{
		Enabled: true, Engine: TTSEngineOpenRouter, APIKey: "k", VoiceID: "alloy",
		Fallback: &config.TTSFallbackConfig{Engine: TTSEngineOpenRouter}, // no voice_id
	}
	client := NewTTSClient(&cfg, "")
	if client == nil {
		t.Fatal("primary engine is fine; client must not be nil")
	}
	if _, wrapped := client.engine.(*FallbackTTSClient); wrapped {
		t.Error("a fallback that failed to initialise was wrapped around the primary")
	}
}

// Nova reports a classified *provider.Error; the fallback decision must follow
// its Retryable flag even when the message carries no status text, and must
// survive wrapping (fmt.Errorf with %w), which is how transcribeWithNova returns it.
func TestIsRetriable_UsesNovaClassification(t *testing.T) {
	retryable := fmt.Errorf("STT request failed: %w", &provider.Error{Status: 429, Retryable: true, Message: "slow down"})
	if !isRetriable(retryable) {
		t.Error("a retryable provider error (429) should trigger the fallback model")
	}
	fatal := fmt.Errorf("STT request failed: %w", &provider.Error{Status: 401, Message: "bad key"})
	if isRetriable(fatal) {
		t.Error("a 401 would fail on the fallback model too; it must not be retried")
	}
	if !isRetriable(errors.New("context deadline exceeded")) {
		t.Error("transport timeouts stay retriable")
	}
}

// TestNovaTTS_CostLookupDefaultsOff pins the default: the lookup costs one extra
// request per synthesis and, as of Nova v0.4.0, resolves nothing for speech
// (OpenRouter returns nulls for gen-tts ids — nova#91). Turning it on is a
// config line when that changes.
func TestNovaTTS_CostLookupDefaultsOff(t *testing.T) {
	cfg := &config.TTSConfig{BaseURL: "https://openrouter.ai/api/v1", APIKey: "k", Model: "m", VoiceID: "v"}
	engine := NewOpenRouterTTSClient(cfg, "")
	if engine == nil {
		t.Fatal("engine did not build")
	}
	if engine.costLookup {
		t.Error("cost_lookup is on by default; it pays for a lookup that resolves nothing")
	}
	on := true
	cfg.CostLookup = &on
	if engine := NewOpenRouterTTSClient(cfg, ""); engine == nil || !engine.costLookup {
		t.Error("cost_lookup: true did not enable the lookup")
	}
}
