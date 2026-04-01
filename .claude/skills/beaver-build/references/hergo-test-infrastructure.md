# her-go Test Infrastructure Map

Current state of test utilities, patterns, and coverage gaps.
**Last updated: 2026-04-01**

---

## Existing Test Helpers

### skills/loader — Database & Proxy Helpers

```go
// newTestDB creates a temp SQLite with seed data (expenses, mood_entries, secrets).
// Returns the DB file path. Cleaned up automatically via t.TempDir().
func newTestDB(t *testing.T) string

// newTestProxy creates a DBProxy backed by a temp test database.
// Registers t.Cleanup(proxy.Close) automatically.
func newTestProxy(t *testing.T) (*DBProxy, string)

// HTTP helpers for testing proxy endpoints
func postJSON(url string, body any) (*http.Response, error)
func putJSON(url, body any) (*http.Response, error)
func doDelete(url string) (*http.Response, error)
```

### skills/loader — Embedding & Sidecar Helpers

```go
// makeTestEmbedding creates a small (4-dim) test embedding vector.
// Fast and deterministic — real embeddings are 384-dim.
func makeTestEmbedding(vals ...float32) []float32

// makeTestSkillsDir creates a temp directory with skill.md fixtures.
// Returns the directory path. Cleaned up via t.TempDir().
func makeTestSkillsDir(t *testing.T, skills ...testSkillSpec) string
```

### skills/skillkit/go — Package State Helpers

```go
// saveAndRestore swaps package-level globals (stdinReader, isStdinPipe,
// osArgs, stdout, exit) with test values and returns a cleanup func.
// Pattern: swap globals → run test → restore originals.
func saveAndRestore(t *testing.T, opts ...saveOpt) func()
```

### compact — Message Factories

```go
// makeMessages creates N fake messages alternating user/assistant,
// each with roughly charsPerMsg characters of content.
// Used to control token estimation precisely.
func makeMessages(n int, charsPerMsg int) []memory.Message
```

---

## Packages WITH Tests (Current Coverage)

| Package | Test Files | Lines | Quality | Notes |
|---------|-----------|-------|---------|-------|
| `skills/loader/` | 6 | ~1,400 | Excellent | Security-focused: SSRF, SQL injection, trust tiers |
| `skills/skillkit/go/` | 4 | ~630 | Good | All public APIs, mock HTTP servers, state isolation |
| `compact/` | 1 | ~345 | Good | Token estimation, compaction thresholds, real DB |
| `tools/` | 3 | ~305 | Good | Tool loading, rendering, drift guards, traces |
| `agent/` | 2 | ~230 | Good | Classifier parsing, prompt manipulation |

**Total: ~2,900 lines of test code across 16 files**

---

## Packages WITHOUT Tests (Coverage Gaps)

### High Priority — Business-Critical Logic

These packages are where bugs hurt the most. Test them first.

| Package | Files | Why It Matters | Test Approach |
|---------|-------|----------------|---------------|
| `memory/` | 3 | Bot's long-term brain. Wrong storage = amnesia or pollution | Real temp SQLite, round-trip tests, search accuracy |
| `tools/save_fact/` | 1 | Most-used tool. Wrong save = garbage in memory DB | Tool handler template + real DB |
| `tools/update_fact/` | 1 | Supersession chains. Wrong update = broken knowledge evolution | Handler + verify old fact marked inactive |
| `scrub/` | 2 | PII protection. Wrong scrub = leaked personal data | Table-driven: each tier, edge cases, round-trips |
| `config/` | 1 | Everything depends on config. Wrong parse = cascade failures | Real temp YAML files, missing fields, defaults |

### Medium Priority — Important but Less Risky

| Package | Files | Why It Matters | Test Approach |
|---------|-------|----------------|---------------|
| `agent/layers/` | 20 | Prompt assembly — wrong layers = broken personality | Verify layer output shape, marker expansion |
| `embed/` | 1 | Embedding client. Wrong vectors = broken similarity search | httptest mock of embedding API |
| `llm/` | 1 | LLM client. Wrong request = wrong model responses | httptest template (see test-templates.md) |
| `persona/` | 2 | Persona evolution. Wrong evolution = personality drift | Verify damping, version tracking |
| `search/` | 2 | Web + book search. Wrong parse = missing information | httptest mock of Tavily/Open Library |
| `weather/` | 1 | Weather integration. Wrong parse = wrong forecasts | httptest mock of Open-Meteo |
| `scheduler/` | 4 | Reminder delivery. Wrong schedule = missed/duplicate reminders | Timer logic, delivery callbacks |

### Low Priority — Hard to Test or Low Risk

| Package | Files | Why It Matters | Test Approach |
|---------|-------|----------------|---------------|
| `bot/` | 8 | Telegram handler. Mostly wiring, hard to unit test | Integration tests with mock bot API |
| `cmd/` | 16 | CLI commands. Mostly Cobra wiring | Smoke tests at most |
| `tui/` | 7 | Terminal UI. Visual, hard to assert on | Skip or minimal |
| `vision/` | 1 | Gemini VLM. External API, expensive | httptest mock only |
| `voice/` | 2 | Piper TTS + Parakeet STT. External processes | Skip (process management) |
| `ocr/` | 1 | OCR extraction. External process | Skip |
| `logger/` | 1 | Structured logging. Trust charmbracelet/log | Skip |

### Individual Tool Handlers (27 tools, 0 tests)

Every tool in `tools/<name>/handler.go` is a user-facing action with zero test
coverage. Priority order by impact:

| Tool | Why | Approach |
|------|-----|----------|
| `save_fact` | Most-called tool, memory quality depends on it | Full handler test + classifier integration |
| `save_self_fact` | Bot's self-knowledge, same gates as save_fact | Shares ExecSaveFact — test the shared path |
| `update_fact` | Supersession chains, importance clamping | Handler test + verify chain integrity |
| `reply` | Final user-facing output, TTS trigger | Verify status callback, markdown escaping |
| `search_facts` | Memory retrieval, context building | Verify query handling, empty results |
| `web_search` | External API integration | httptest mock of Tavily |
| `think` | No side effects (just logging) | Skip or trivial |
| `done` | No side effects | Skip |
| `no_action` | No side effects | Skip |

---

## Testing Patterns Already in Use

### Standard Patterns (Keep Using)

- **Table-driven tests** — `[]struct{ name; input; want }` + `t.Run`
- **t.Helper()** — on all helper functions
- **t.TempDir()** — for disposable file system fixtures
- **t.Cleanup()** — for deferred resource teardown
- **Real SQLite** — temp DB files, not mocked
- **httptest.NewServer** — for HTTP integration tests
- **t.Logf()** — for debug output (only shown with `go test -v`)

### Assertion Style (Keep Consistent)

```go
// Standard pattern in this repo:
if got != want {
    t.Errorf("FuncName(%v) = %v, want %v", input, got, want)
}

// For fatal setup errors:
if err != nil {
    t.Fatalf("setup: %v", err)
}

// For substring checks (common in tool handler tests):
if !strings.Contains(result, "expected") {
    t.Errorf("result = %q, want substring %q", result, "expected")
}
```

No testify, no gomock, no assertion libraries. Standard library only.

---

## Test Helpers to Build

These don't exist yet but would reduce friction for new tests.

### 1. Shared `newTestStore` (for memory package tests)

```go
// testutil/store.go (or inline in memory package tests)
func newTestStore(t *testing.T) *memory.Store {
    t.Helper()
    dbPath := filepath.Join(t.TempDir(), "test.db")
    store, err := memory.NewStore(dbPath)
    if err != nil {
        t.Fatalf("NewStore: %v", err)
    }
    t.Cleanup(func() { store.Close() })
    return store
}
```

### 2. Shared `newTestContext` (for tool handler tests)

```go
// testutil/context.go (or inline in tool package tests)
func newTestContext(t *testing.T) *tools.Context {
    t.Helper()
    store := newTestStore(t)
    cfg := config.Default() // needs a Default() that returns safe test config
    return &tools.Context{
        Store: store,
        Cfg:   cfg,
    }
}
```

### 3. `config.Default()` — Test-Safe Config

A function that returns a valid `config.Config` with safe defaults (no real API
keys, no real bot tokens). Needed by any test that touches `tools.Context`.

### 4. Fake LLM Server

```go
// testutil/llm.go
// Returns an httptest.Server that responds with canned LLM completions.
// Useful for testing tool handlers that invoke the classifier or agent.
func newFakeLLMServer(t *testing.T, response string) *httptest.Server
```

---

## Running Tests

```bash
# Run all tests
go test ./...

# Run a specific package
go test ./memory/...
go test ./tools/save_fact/...

# Run with verbose output (shows t.Log messages)
go test -v ./compact/...

# Run with race detector
go test -race ./...

# Run with coverage
go test -coverprofile=coverage.out ./...
go tool cover -html=coverage.out

# Run a specific test by name
go test -run TestParseClassifierResponse ./agent/...

# Run table-driven subtests by name
go test -run TestParseClassifierResponse/plain_SAVE ./agent/...
```
