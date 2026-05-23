package voice

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"her/config"
)

// makeVoiceCfg builds a minimal VoiceConfig pointing at a test server URL.
func makeVoiceCfg(engine, baseURL, apiKey string) config.VoiceConfig {
	return config.VoiceConfig{
		Enabled: true,
		STT: config.STTConfig{
			Engine:  engine,
			BaseURL: baseURL,
			Model:   "test-model",
			APIKey:  apiKey,
		},
	}
}

func TestIsAvailable_LocalParakeet(t *testing.T) {
	// Spin up a fake parakeet server that responds OK to /healthz.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusOK)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	cfg := makeVoiceCfg(config.STTEngineParakeet, srv.URL, "")
	c := NewClient(&cfg, "")
	if c == nil {
		t.Fatal("NewClient returned nil")
	}
	if !c.IsAvailable() {
		t.Error("expected IsAvailable() = true for a healthy local server")
	}
}

func TestIsAvailable_LocalParakeet_Down(t *testing.T) {
	// Point at a port nothing is listening on.
	cfg := makeVoiceCfg(config.STTEngineParakeet, "http://127.0.0.1:19999", "")
	c := NewClient(&cfg, "")
	if c == nil {
		t.Fatal("NewClient returned nil")
	}
	if c.IsAvailable() {
		t.Error("expected IsAvailable() = false for an unreachable local server")
	}
}

func TestIsAvailable_RemoteWhisper_SkipsHealthCheck(t *testing.T) {
	// Remote engines must return true without making any network call.
	// Use an invalid URL — if a request is made, the test will fail via err.
	cfg := makeVoiceCfg(config.STTEngineWhisper, "http://0.0.0.0:0", "sk-test-key")
	c := NewClient(&cfg, "")
	if c == nil {
		t.Fatal("NewClient returned nil")
	}
	if !c.IsAvailable() {
		t.Error("expected IsAvailable() = true for remote engine (health check should be skipped)")
	}
}

func TestTranscribe_BearerHeaderSet(t *testing.T) {
	// Verify that the Authorization header is sent when apiKey is non-empty.
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"hello world"}`))
	}))
	defer srv.Close()

	cfg := makeVoiceCfg(config.STTEngineWhisper, srv.URL, "sk-test-key")
	c := NewClient(&cfg, "")
	if c == nil {
		t.Fatal("NewClient returned nil")
	}

	_, err := c.Transcribe([]byte("fake-audio"), "voice.ogg")
	if err != nil {
		t.Fatalf("Transcribe returned unexpected error: %v", err)
	}
	if gotAuth != "Bearer sk-test-key" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer sk-test-key")
	}
}

func TestTranscribe_NoAuthHeader_LocalEngine(t *testing.T) {
	// Local engines must not send an Authorization header.
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"hello"}`))
	}))
	defer srv.Close()

	cfg := makeVoiceCfg(config.STTEngineParakeet, srv.URL, "")
	c := NewClient(&cfg, "")
	if c == nil {
		t.Fatal("NewClient returned nil")
	}

	_, err := c.Transcribe([]byte("fake-audio"), "voice.ogg")
	if err != nil {
		t.Fatalf("Transcribe returned unexpected error: %v", err)
	}
	if gotAuth != "" {
		t.Errorf("Authorization header = %q, want empty for local engine", gotAuth)
	}
}

func TestTranscribe_FallbackAPIKey(t *testing.T) {
	// When STTConfig.APIKey is empty, NewClient should use the fallback key.
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"hello"}`))
	}))
	defer srv.Close()

	cfg := makeVoiceCfg(config.STTEngineWhisper, srv.URL, "") // no explicit api_key
	c := NewClient(&cfg, "sk-openrouter-fallback")
	if c == nil {
		t.Fatal("NewClient returned nil")
	}

	_, err := c.Transcribe([]byte("fake-audio"), "voice.ogg")
	if err != nil {
		t.Fatalf("Transcribe returned unexpected error: %v", err)
	}
	if !strings.HasPrefix(gotAuth, "Bearer ") {
		t.Errorf("Authorization header = %q, want Bearer prefix", gotAuth)
	}
	if gotAuth != "Bearer sk-openrouter-fallback" {
		t.Errorf("Authorization header = %q, want %q", gotAuth, "Bearer sk-openrouter-fallback")
	}
}
