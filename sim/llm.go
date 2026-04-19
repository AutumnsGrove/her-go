package sim

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
)

// FakeLLM is an httptest.Server that speaks the OpenAI chat-completions
// JSON the bot's llm.Client expects. It maps incoming requests to
// canned responses via a Script — scenarios register
// "if prompt contains X, reply with Y" rules before running.
//
// This is the trust boundary where we MUST mock: we can't call real
// OpenRouter from a deterministic test. Everything else (scheduler,
// store, mood agent loop) runs against real code.
type FakeLLM struct {
	t      TestingT
	server *httptest.Server

	mu    sync.Mutex
	rules []scriptRule
	// calls records every request the bot made against the fake —
	// scenarios can assert on prompt content, model choice, etc.
	calls []FakeLLMCall
}

// FakeLLMCall is a captured request for post-hoc assertion.
type FakeLLMCall struct {
	Model    string
	Messages []ChatMsg
	Raw      []byte
}

// ChatMsg mirrors the OpenAI message shape minus the bits the bot
// never inspects (tool_calls, name, etc.).
type ChatMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type scriptRule struct {
	// MatchContent — if non-empty, the rule fires when the concatenated
	// user+system content contains this substring. Empty matches all.
	MatchContent string

	// Response is the assistant's "content" returned verbatim.
	Response string

	// Once is true if the rule should fire at most once. After firing,
	// it's removed from the rule list so subsequent calls fall through.
	Once bool
}

// NewFakeLLM spins up an httptest.Server configured for OpenRouter's
// chat-completions endpoint. Remember to call Close() — tests typically
// register it with t.Cleanup.
func NewFakeLLM(t TestingT) *FakeLLM {
	t.Helper()
	f := &FakeLLM{t: t}
	f.server = httptest.NewServer(http.HandlerFunc(f.handle))
	t.Cleanup(f.Close)
	return f
}

// URL returns the base URL to pass into llm.Client's BaseURL. The
// llm.Client appends "/chat/completions" on its own.
func (f *FakeLLM) URL() string { return f.server.URL }

// Close shuts down the underlying httptest server.
func (f *FakeLLM) Close() { f.server.Close() }

// Script adds a rule: whenever a request's prompt contains match, reply
// with response. Later rules override earlier ones (last-in, first-out).
// Pass an empty match to catch-all.
func (f *FakeLLM) Script(match, response string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = append([]scriptRule{{MatchContent: match, Response: response}}, f.rules...)
}

// ScriptOnce is like Script but the rule self-removes after a single
// firing. Useful for "the first call returns X, the second Y" patterns.
func (f *FakeLLM) ScriptOnce(match, response string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rules = append([]scriptRule{{MatchContent: match, Response: response, Once: true}}, f.rules...)
}

// Calls returns every request the bot made during the scenario.
func (f *FakeLLM) Calls() []FakeLLMCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]FakeLLMCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// handle is the httptest handler. It logs the call, picks a matching
// rule, and writes a minimal OpenAI-shaped response. Falls through to
// a generic "OK" message if no rule matches so tests never 500 on an
// unscripted call (they can fail assertions instead of hanging).
func (f *FakeLLM) handle(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	defer r.Body.Close()

	var req struct {
		Model    string    `json:"model"`
		Messages []ChatMsg `json:"messages"`
	}
	_ = json.Unmarshal(body, &req)

	f.mu.Lock()
	f.calls = append(f.calls, FakeLLMCall{
		Model:    req.Model,
		Messages: req.Messages,
		Raw:      body,
	})

	// Pick the first matching rule.
	concat := concatContent(req.Messages)
	var reply string
	var matchedIdx = -1
	for i, rule := range f.rules {
		if rule.MatchContent == "" || strings.Contains(concat, rule.MatchContent) {
			reply = rule.Response
			matchedIdx = i
			break
		}
	}
	if matchedIdx >= 0 && f.rules[matchedIdx].Once {
		f.rules = append(f.rules[:matchedIdx], f.rules[matchedIdx+1:]...)
	}
	f.mu.Unlock()

	if reply == "" {
		reply = "ok"
	}

	// Minimal OpenAI response shape. llm.Client reads Choices[0].Message.Content.
	resp := map[string]any{
		"id":      "sim-completion",
		"object":  "chat.completion",
		"model":   req.Model,
		"choices": []map[string]any{{
			"index":         0,
			"finish_reason": "stop",
			"message": map[string]any{
				"role":    "assistant",
				"content": reply,
			},
		}},
		"usage": map[string]any{
			"prompt_tokens":     len(concat) / 4,
			"completion_tokens": len(reply) / 4,
			"total_tokens":      (len(concat) + len(reply)) / 4,
		},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func concatContent(msgs []ChatMsg) string {
	var b bytes.Buffer
	for _, m := range msgs {
		b.WriteString(m.Content)
		b.WriteByte('\n')
	}
	return b.String()
}

// DumpCalls prints a human-readable transcript of every LLM call for a
// scenario. Useful when debugging why a scenario produces the wrong
// Telegram transcript.
func (f *FakeLLM) DumpCalls(out io.Writer) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, c := range f.calls {
		fmt.Fprintf(out, "── call %d — model=%s ──\n", i+1, c.Model)
		for _, m := range c.Messages {
			fmt.Fprintf(out, "[%s] %s\n", m.Role, m.Content)
		}
		fmt.Fprintln(out)
	}
}
