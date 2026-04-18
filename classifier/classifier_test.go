package classifier

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"her/llm"
	"her/memory"
)

func TestParseResponse(t *testing.T) {
	tests := []struct {
		name        string
		response    string
		wantAllowed bool
		wantType    string
		wantReason  string
		wantRewrite string
	}{
		{
			name:        "plain SAVE",
			response:    "SAVE",
			wantAllowed: true,
			wantType:    "SAVE",
		},
		{
			name:        "SAVE with explanation",
			response:    "SAVE — real user preference",
			wantAllowed: true,
			wantType:    "SAVE",
		},
		{
			// FICTIONAL was removed — too many false positives on real past events.
			// The agent model's own judgment handles fiction-filtering well enough.
			// Like INFERRED, the parser no longer recognizes it → fail-open.
			name:        "FICTIONAL removed — fails open",
			response:    "FICTIONAL — in-game event from Elden Ring",
			wantAllowed: true,
			wantType:    "SAVE",
		},
		{
			// INFERRED was removed — memory agent reads raw conversation text so
			// reasonable summarization is always acceptable.
			name:        "INFERRED no longer known — fails open",
			response:    `INFERRED REWRITE: "User adopted their cat Bean from a Portland shelter"`,
			wantAllowed: true,
			wantType:    "SAVE",
		},
		{
			// MOOD_NOT_FACT removed — mood tracking moved to junk drawer.
			name:        "MOOD_NOT_FACT removed — fails open",
			response:    "MOOD_NOT_FACT — transient frustration",
			wantAllowed: true,
			wantType:    "SAVE",
		},
		{
			name:        "LOW_VALUE hard reject",
			response:    "LOW_VALUE — too vague to be actionable",
			wantAllowed: false,
			wantType:    "LOW_VALUE",
			wantReason:  "too vague to be actionable",
		},
		{
			name:        "PASS allowed",
			response:    "PASS",
			wantAllowed: true,
			wantType:    "PASS",
		},
		{
			name:        "STYLE_ISSUE rejected",
			response:    "STYLE_ISSUE — opens with hollow affirmation",
			wantAllowed: false,
			wantType:    "STYLE_ISSUE",
			wantReason:  "opens with hollow affirmation",
		},
		{
			name:        "unparseable response fails open",
			response:    "I think this fact is fine to save",
			wantAllowed: true,
			wantType:    "SAVE",
		},
		{
			name:        "multiline — only first line checked",
			response:    "LOW_VALUE — too vague\nThe fact doesn't tell us anything useful",
			wantAllowed: false,
			wantType:    "LOW_VALUE",
			wantReason:  "too vague",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := parseResponse(tt.response)
			if v.Allowed != tt.wantAllowed {
				t.Errorf("Allowed = %v, want %v", v.Allowed, tt.wantAllowed)
			}
			if v.Type != tt.wantType {
				t.Errorf("Type = %q, want %q", v.Type, tt.wantType)
			}
			if tt.wantReason != "" && v.Reason != tt.wantReason {
				t.Errorf("Reason = %q, want %q", v.Reason, tt.wantReason)
			}
			if tt.wantRewrite != "" && v.Rewrite != tt.wantRewrite {
				t.Errorf("Rewrite = %q, want %q", v.Rewrite, tt.wantRewrite)
			}
		})
	}
}

func TestExtractRewrite(t *testing.T) {
	tests := []struct {
		name    string
		line    string
		verdict string
		want    string
	}{
		{
			name:    "quoted rewrite",
			line:    `FICTIONAL REWRITE: "User prefers bleed builds"`,
			verdict: "FICTIONAL",
			want:    "User prefers bleed builds",
		},
		{
			name:    "unquoted rewrite",
			line:    `INFERRED REWRITE: User adopted cat Bean`,
			verdict: "INFERRED",
			want:    "User adopted cat Bean",
		},
		{
			name:    "no rewrite present",
			line:    "LOW_VALUE — too vague",
			verdict: "LOW_VALUE",
			want:    "",
		},
		{
			name:    "rewrite with mixed case keyword",
			line:    `LOW_VALUE Rewrite: "User enjoys surreal fiction like Piranesi"`,
			verdict: "LOW_VALUE",
			want:    "User enjoys surreal fiction like Piranesi",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractRewrite(tt.line, tt.verdict)
			if got != tt.want {
				t.Errorf("extractRewrite() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestRejectionMessage(t *testing.T) {
	t.Run("soft verdict with rewrite suggests text", func(t *testing.T) {
		v := Verdict{Allowed: false, Type: "FICTIONAL", Rewrite: "User prefers bleed builds in Elden Ring"}
		msg := RejectionMessage(v)
		if msg == "" {
			t.Error("expected non-empty rejection message")
		}
		if !strings.Contains(msg, "User prefers bleed builds") {
			t.Errorf("expected rewrite text in message, got: %s", msg)
		}
	})

	t.Run("unknown verdict type falls back to reason", func(t *testing.T) {
		v := Verdict{Allowed: false, Type: "UNKNOWN_VERDICT", Reason: "some reason"}
		msg := RejectionMessage(v)
		if !strings.Contains(msg, "some reason") {
			t.Errorf("expected reason in fallback message, got: %s", msg)
		}
	})
}

// mockClassifierServer starts a test HTTP server that returns a fixed response
// body for any request. It mimics the OpenRouter /chat/completions endpoint
// just enough for the classifier client to parse it.
func mockClassifierServer(t *testing.T, response string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The llm client expects an OpenAI-compatible response body.
		// We only need the choices[0].message.content field.
		body := map[string]interface{}{
			"choices": []map[string]interface{}{
				{"message": map[string]string{"content": response}, "finish_reason": "stop"},
			},
			"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15},
			"model": "test-model",
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(body)
	}))
}

func TestCheck(t *testing.T) {
	t.Run("nil client fails open", func(t *testing.T) {
		v := Check(nil, "memory", "some content", nil)
		if !v.Allowed {
			t.Error("nil classifier LLM should fail open (Allowed=true)")
		}
		if v.Type != "SAVE" {
			t.Errorf("Type = %q, want SAVE", v.Type)
		}
	})

	t.Run("unknown writeType fails open", func(t *testing.T) {
		srv := mockClassifierServer(t, "SAVE")
		defer srv.Close()
		client := llm.NewClient(srv.URL, "test-key", "test-model", 0, 64)

		v := Check(client, "nonexistent_type", "some memory", nil)
		if !v.Allowed {
			t.Error("unknown writeType should fail open (Allowed=true)")
		}
	})

	t.Run("LLM returns SAVE — write allowed", func(t *testing.T) {
		srv := mockClassifierServer(t, "SAVE — clear user preference")
		defer srv.Close()
		client := llm.NewClient(srv.URL, "test-key", "test-model", 0, 64)

		v := Check(client, "memory", "Autumn prefers stealth builds in Elden Ring", nil)
		if !v.Allowed {
			t.Errorf("SAVE response should allow write, got Allowed=false (type=%s reason=%s)", v.Type, v.Reason)
		}
	})

	t.Run("LLM returns LOW_VALUE — write rejected", func(t *testing.T) {
		srv := mockClassifierServer(t, "LOW_VALUE — too generic to be useful")
		defer srv.Close()
		client := llm.NewClient(srv.URL, "test-key", "test-model", 0, 64)

		v := Check(client, "memory", "Autumn likes things", nil)
		if v.Allowed {
			t.Error("LOW_VALUE response should reject write (Allowed=false)")
		}
		if v.Type != "LOW_VALUE" {
			t.Errorf("Type = %q, want LOW_VALUE", v.Type)
		}
	})

	t.Run("LLM returns PASS — reply style approved", func(t *testing.T) {
		srv := mockClassifierServer(t, "PASS")
		defer srv.Close()
		client := llm.NewClient(srv.URL, "test-key", "test-model", 0, 64)

		v := Check(client, "reply", "That's so interesting!", nil)
		if !v.Allowed {
			t.Error("PASS response should allow reply (Allowed=true)")
		}
		if v.Type != "PASS" {
			t.Errorf("Type = %q, want PASS", v.Type)
		}
	})

	t.Run("snippet messages included in context", func(t *testing.T) {
		// Verify the snippet is forwarded to the LLM — check that the request
		// body contains the message content.
		var capturedBody string
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req map[string]interface{}
			json.NewDecoder(r.Body).Decode(&req)
			b, _ := json.Marshal(req)
			capturedBody = string(b)
			body := map[string]interface{}{
				"choices": []map[string]interface{}{
					{"message": map[string]string{"content": "SAVE"}, "finish_reason": "stop"},
				},
				"usage": map[string]int{"prompt_tokens": 10, "completion_tokens": 2, "total_tokens": 12},
				"model": "test-model",
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(body)
		}))
		defer srv.Close()
		client := llm.NewClient(srv.URL, "test-key", "test-model", 0, 64)

		snippet := []memory.Message{
			{Role: "user", ContentRaw: "i built a grocery scraper"},
			{Role: "assistant", ContentRaw: "that's a real project!"},
		}
		Check(client, "memory", "Autumn built a web scraper", snippet)

		if !strings.Contains(capturedBody, "grocery scraper") {
			t.Errorf("expected snippet content in LLM request body, got: %s", capturedBody)
		}
	})

	t.Run("server error fails open", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer srv.Close()
		client := llm.NewClient(srv.URL, "test-key", "test-model", 0, 64)

		v := Check(client, "memory", "some memory", nil)
		if !v.Allowed {
			t.Error("LLM error should fail open (Allowed=true)")
		}
	})
}
