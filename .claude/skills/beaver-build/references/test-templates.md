# Go Test Templates — her-go

Concrete test templates matching the patterns already established in this repo.
Copy, adapt, and fill in — don't start from scratch.

## File Location Convention

Tests live right next to the code they test, in the same package:

```
tools/update_fact/
├── handler.go
└── handler_test.go     ← same package (update_fact), same directory

compact/
├── compact.go
└── compact_test.go     ← same package (compact), same directory
```

For black-box testing of a package's public API only, use the `_test` suffix:

```go
package memory_test  // can only access exported names from memory/
```

---

## Template 1: Table-Driven Unit Test (Pure Function)

Use for: parsers, formatters, validators, estimators — anything that takes
input and returns output with no side effects.

This is the most common pattern in the repo. See `classifier_test.go`,
`trace_test.go`, `trust_test.go` for real examples.

```go
package mypackage

import "testing"

func TestParseWidgetResponse(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		wantType string
		wantOK   bool
	}{
		{
			name:     "valid response parses correctly",
			input:    "ACCEPT — looks good",
			wantType: "ACCEPT",
			wantOK:   true,
		},
		{
			name:     "reject with reason captures reason",
			input:    "REJECT — too vague",
			wantType: "REJECT",
			wantOK:   false,
		},
		{
			name:     "empty input fails open",
			input:    "",
			wantType: "ACCEPT",
			wantOK:   true,
		},
		{
			name:     "unparseable input fails open",
			input:    "I think this is fine",
			wantType: "ACCEPT",
			wantOK:   true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := parseWidgetResponse(tt.input)
			if got.Type != tt.wantType {
				t.Errorf("Type = %q, want %q", got.Type, tt.wantType)
			}
			if got.OK != tt.wantOK {
				t.Errorf("OK = %v, want %v", got.OK, tt.wantOK)
			}
		})
	}
}
```

**Key points:**
- Struct fields match the function's inputs and expected outputs
- Each case has a descriptive `name` that explains what's being tested
- Assertions use `t.Errorf` with `got, want` format
- No setup/teardown needed — pure functions are self-contained

---

## Template 2: SQLite Integration Test (Database Operations)

Use for: fact storage, context queries, compaction, anything that touches
`memory.Store` or raw SQL. **Always use a real temp database**, never mock it.

See `compact_test.go` and `dbproxy_test.go` for real examples.

```go
package memory

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// newTestStore creates a temporary Store backed by a real SQLite database.
// The database is automatically cleaned up when the test finishes.
//
// t.Helper() marks this as a helper so test failures report the caller's
// line number, not this function's.
func newTestStore(t *testing.T) *Store {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	store, err := NewStore(dbPath)
	if err != nil {
		t.Fatalf("NewStore(%q): %v", dbPath, err)
	}
	t.Cleanup(func() { store.Close() })

	return store
}

func TestSaveFact_RoundTrip(t *testing.T) {
	store := newTestStore(t)

	// Arrange
	fact := Fact{
		Content:    "User's cat is named Bean",
		Category:   "pets",
		Importance: 7,
	}

	// Act — save it
	id, err := store.SaveFact(fact)
	if err != nil {
		t.Fatalf("SaveFact: %v", err)
	}

	// Act — read it back
	got, err := store.GetFact(id)
	if err != nil {
		t.Fatalf("GetFact(%d): %v", id, err)
	}

	// Assert
	if got.Content != fact.Content {
		t.Errorf("Content = %q, want %q", got.Content, fact.Content)
	}
	if got.Category != fact.Category {
		t.Errorf("Category = %q, want %q", got.Category, fact.Category)
	}
}

func TestSaveFact_DuplicateDetection(t *testing.T) {
	store := newTestStore(t)

	fact := Fact{Content: "User likes coffee", Category: "preferences"}

	// Save twice
	id1, _ := store.SaveFact(fact)
	id2, _ := store.SaveFact(fact)

	// Should either deduplicate or link them
	if id1 == id2 {
		t.Log("duplicate was deduplicated (same ID returned)")
	}
	// ... assert whatever the expected behavior is
}

func TestSearchFacts_EmptyDB(t *testing.T) {
	store := newTestStore(t)

	// Searching an empty database should return empty slice, not error
	results, err := store.SearchFacts("anything")
	if err != nil {
		t.Fatalf("SearchFacts on empty DB: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("got %d results, want 0", len(results))
	}
}
```

**Key points:**
- `newTestStore` helper uses `t.TempDir()` (auto-cleaned) and `t.Cleanup`
- `t.Helper()` on every helper function
- `t.Fatalf` for setup failures (test can't continue), `t.Errorf` for assertions
- Real SQLite, not mocked — catches real SQL bugs

---

## Template 3: HTTP Integration Test (External API Client)

Use for: LLM client, search API, weather API — anything that makes HTTP calls.
Mock the server, not the client.

See `db_test.go` (skillkit) and `proxy_test.go` for real examples.

```go
package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClient_ChatSendsCorrectPayload(t *testing.T) {
	// Arrange: fake API server that captures the request
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify headers
		if got := r.Header.Get("Authorization"); got != "Bearer test-key" {
			t.Errorf("Authorization = %q, want %q", got, "Bearer test-key")
		}

		// Capture request body
		json.NewDecoder(r.Body).Decode(&gotBody)

		// Return canned response
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{
				{"message": map[string]string{
					"role":    "assistant",
					"content": "Hello!",
				}},
			},
		})
	}))
	defer srv.Close()

	// Act: call client pointed at fake server
	client := NewClient(srv.URL, "test-key", "test-model")
	resp, err := client.Chat(context.Background(), []Message{
		{Role: "user", Content: "Hi"},
	})

	// Assert
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "Hello!" {
		t.Errorf("Content = %q, want %q", resp.Content, "Hello!")
	}

	// Verify the request shape
	if model, ok := gotBody["model"].(string); !ok || model != "test-model" {
		t.Errorf("request model = %v, want %q", gotBody["model"], "test-model")
	}
}

func TestClient_HandlesAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		w.Write([]byte(`{"error": {"message": "rate limited"}}`))
	}))
	defer srv.Close()

	client := NewClient(srv.URL, "test-key", "test-model")
	_, err := client.Chat(context.Background(), []Message{
		{Role: "user", Content: "Hi"},
	})

	if err == nil {
		t.Fatal("expected error for 429 response, got nil")
	}
}
```

**Key points:**
- `httptest.NewServer` creates a real HTTP server on localhost
- Test both the request shape (what the client sends) AND the response handling
- Test error paths: 429, 500, malformed JSON, timeout
- `defer srv.Close()` ensures cleanup even if test panics

---

## Template 4: Tool Handler Test

Use for: any tool in `tools/<name>/handler.go`. Tool handlers take a JSON
string + `*tools.Context` and return a string result.

The challenge is building a realistic `tools.Context`. Start with the minimum
viable context and add fields as the handler needs them.

```go
package save_fact

import (
	"encoding/json"
	"strings"
	"testing"

	"her/config"
	"her/memory"
	"her/tools"
)

// newTestContext creates a minimal tools.Context backed by a real temp database.
// Add fields as handlers need them — don't build a god object.
func newTestContext(t *testing.T) *tools.Context {
	t.Helper()

	store := newTestStore(t) // see Template 2 above

	return &tools.Context{
		Store: store,
		Cfg:   config.Default(), // or a test config
	}
}

func TestHandle_SavesValidFact(t *testing.T) {
	ctx := newTestContext(t)

	args, _ := json.Marshal(map[string]any{
		"fact":       "User's cat is named Bean",
		"category":   "pets",
		"importance": 7,
	})

	// Act
	result := Handle(string(args), ctx)

	// Assert — handler should confirm the save
	if !strings.Contains(result, "saved") && !strings.Contains(result, "Saved") {
		t.Errorf("result = %q, want confirmation of save", result)
	}
}

func TestHandle_RejectsStyleBlockedFact(t *testing.T) {
	ctx := newTestContext(t)

	// "It's important to note" is in the style blocklist
	args, _ := json.Marshal(map[string]any{
		"fact":       "It's important to note that user likes tea",
		"category":   "preferences",
		"importance": 5,
	})

	result := Handle(string(args), ctx)

	if !strings.Contains(result, "rejected") {
		t.Errorf("result = %q, want rejection for style-blocked fact", result)
	}
}

func TestHandle_ClampsImportance(t *testing.T) {
	tests := []struct {
		name           string
		importance     int
		wantClamped    bool // just verify it doesn't error
	}{
		{"below minimum clamps to 1", -5, true},
		{"above maximum clamps to 10", 99, true},
		{"within range passes through", 5, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := newTestContext(t)
			args, _ := json.Marshal(map[string]any{
				"fact":       "User likes Go",
				"category":   "tech",
				"importance": tt.importance,
			})

			result := Handle(string(args), ctx)

			// Should not error regardless of importance value
			if strings.Contains(result, "error") {
				t.Errorf("importance=%d caused error: %s", tt.importance, result)
			}
		})
	}
}

func TestHandle_InvalidJSON(t *testing.T) {
	ctx := newTestContext(t)

	result := Handle("not json at all", ctx)

	if !strings.Contains(result, "error") {
		t.Errorf("result = %q, want error for invalid JSON", result)
	}
}
```

**Key points:**
- `newTestContext` builds a minimal `tools.Context` — only set what the handler uses
- Test the happy path, rejection paths, edge cases, and invalid input
- Handler returns a string, so assertions use `strings.Contains`
- Table-driven for cases that vary one parameter (like importance clamping)

---

## Template 5: Config/YAML Loading Test

Use for: config parsing, tool YAML loading, any file-based initialization.

See `loader_test.go` (tools) and `skill_test.go` for real examples.

```go
package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoad_ValidConfig(t *testing.T) {
	// Arrange: write a minimal valid config to a temp file
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	err := os.WriteFile(cfgPath, []byte(`
openrouter_api_key: "test-key"
telegram_bot_token: "test-token"
chat_model: "test/model"
max_history_tokens: 4000
`), 0644)
	if err != nil {
		t.Fatalf("write config: %v", err)
	}

	// Act
	cfg, err := Load(cfgPath)

	// Assert
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.ChatModel != "test/model" {
		t.Errorf("ChatModel = %q, want %q", cfg.ChatModel, "test/model")
	}
	if cfg.MaxHistoryTokens != 4000 {
		t.Errorf("MaxHistoryTokens = %d, want 4000", cfg.MaxHistoryTokens)
	}
}

func TestLoad_MissingRequiredField(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")

	// Missing openrouter_api_key
	err := os.WriteFile(cfgPath, []byte(`
telegram_bot_token: "test-token"
`), 0644)
	if err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err = Load(cfgPath)
	if err == nil {
		t.Fatal("expected error for missing required field, got nil")
	}
}

func TestLoad_FileNotFound(t *testing.T) {
	_, err := Load("/nonexistent/config.yaml")
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}
```

---

## Template 6: PII Scrubbing Test (Security-Critical)

Use for: scrubber, any code that handles sensitive data. Test thoroughly —
wrong scrubbing means leaked personal data.

```go
package scrub

import "testing"

func TestScrub_HardIdentifiersRedacted(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string // or wantContains / wantNotContains
	}{
		{
			name:  "SSN redacted",
			input: "My SSN is 123-45-6789",
			want:  "My SSN is [REDACTED]",
		},
		{
			name:  "credit card redacted",
			input: "Card: 4111 1111 1111 1111",
			want:  "Card: [REDACTED]",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Scrub(tt.input)
			if got != tt.want {
				t.Errorf("Scrub(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestScrub_NamesPreservedInTier1(t *testing.T) {
	// Names should pass through in Tier 1 (privacy level: names allowed)
	input := "My friend Sarah said hi"
	got := Scrub(input)

	if got != input {
		t.Errorf("names should be preserved, got %q", got)
	}
}

func TestScrub_ContactInfoTokenized(t *testing.T) {
	// Phone numbers should be tokenized (replaced with vault tokens),
	// NOT redacted — they need to be deanonymizable in responses.
	input := "Call me at 503-555-1234"
	got := Scrub(input)

	if got == input {
		t.Error("phone number should be tokenized, got original string")
	}
	if strings.Contains(got, "503-555-1234") {
		t.Error("phone number still present in scrubbed output")
	}
	// Should contain a vault token like [PHONE_1], not [REDACTED]
	if strings.Contains(got, "[REDACTED]") {
		t.Error("phone should be tokenized (reversible), not redacted (permanent)")
	}
}
```

**Key points:**
- Security-critical code deserves exhaustive edge cases
- Test each PII tier separately: hard redaction, tokenization, passthrough
- Test that reversible scrubbing IS reversible (round-trip with deanonymize)
- Test with realistic input, not just isolated patterns

---

## Template 7: Drift Guard Test

Use for: catching sync issues between related data structures (e.g., every
tool in the registry should have a trace template, every hot tool should have
a hint). Prevents things from silently getting out of sync.

See `render_test.go` for real examples.

```go
package tools

import "testing"

func TestEveryToolHasTraceTemplate(t *testing.T) {
	// Load all registered tools
	allTools := Registry()

	for _, tool := range allTools {
		name := tool.Function.Name
		t.Run(name, func(t *testing.T) {
			// FormatTrace should not return empty for any registered tool
			trace := FormatTrace(name, `{}`, "result")
			if trace == "" {
				t.Errorf("tool %q has no trace template", name)
			}
		})
	}
}

func TestEveryCategory_HasAtLeastOneTool(t *testing.T) {
	categories := Categories()
	allTools := Registry()

	for _, cat := range categories {
		t.Run(cat.Name, func(t *testing.T) {
			found := false
			for _, tool := range allTools {
				if tool.Category == cat.Name {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("category %q has no tools assigned", cat.Name)
			}
		})
	}
}
```

**Key points:**
- Drift guards are cheap to write and catch real bugs
- They validate invariants: "every X should have a corresponding Y"
- Run them against the actual loaded data, not hardcoded lists
- When they fail, the fix is obvious: add the missing mapping

---

## Self-Review Checklist

Before marking tests as done, run through this:

```
[ ] Tests describe behavior, not implementation details
    → "rejects fact with style-blocked phrase" not "calls styleBlockCheck"

[ ] Each test has one clear reason to fail
    → If a test checks 5 things, split it into 5 tests

[ ] Test names explain what breaks when they fail
    → TestScrub_PreservesNamesInTier1, not TestScrub3

[ ] Table-driven tests used for multiple input/output cases
    → Don't copy-paste the same test with different inputs

[ ] t.Helper() on every helper function
    → Failures should point to the test, not the helper

[ ] Mocks limited to external boundaries
    → Real SQLite, real file system. Mock only APIs you can't call.

[ ] Error messages show got + want + context
    → t.Errorf("Type = %q, want %q", got.Type, want)

[ ] Error paths tested, not just happy paths
    → Invalid JSON, missing fields, DB errors, API 500s

[ ] go test -race passes
    → Catches data races in concurrent code

[ ] go vet passes
    → Catches subtle bugs the compiler misses
```
