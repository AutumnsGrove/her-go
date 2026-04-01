# Go Testing Patterns — her-go

## The Testing Trophy (Go Edition)

```
                    E2E
            (Few: bot integration,
             full conversation flow)

              Integration
          (Many: real DB, httptest,
           multi-package — confidence
                lives here)

                Unit
          (Some: pure functions,
           parsers, formatters)

            Static Analysis
        (Always on: go vet, compiler)
```

Go's compiler and type system are your static analysis layer — they catch a huge
class of bugs that JS/Python find at runtime. That's your foundation for free.

**Integration tests are where confidence lives.** In Go, that means:
- Real SQLite databases (temp files, not mocks)
- Real HTTP servers (`httptest.NewServer`)
- Real package interactions (tool handler → memory store → DB)

Unit tests are fast and focused but narrow. E2E tests (full bot conversations)
are slow and fragile. Integration tests hit the sweet spot.

---

## The Confidence Test

One question cuts through analysis paralysis:

> **"Would I notice if this broke in production?"**

- If YES → test it.
- If NO → skip it.
- If MAYBE → test it lightly (happy path only).

### Follow-up Questions (Kent Beck)

| Question | What it tells you |
|----------|-------------------|
| Does this test fail when the feature breaks? | If no, the test is useless |
| Does this test pass when the feature works? | If flaky, fix or delete it |
| Does this test resemble how the code is used? | If no, it's testing implementation |
| Is this the simplest test that catches this bug? | If no, simplify it |

---

## What to Test — Decision Tables

### Skip (not worth the dam materials)

| What | Why | Example in her-go |
|------|-----|-------------------|
| Trivial getters | Zero logic, compiler-checked | `Config.Model()` |
| Framework plumbing | Trust the framework | Cobra command routing |
| Implementation details | Breaks on safe refactors | Internal field layout |
| Pure logging | Trust charmbracelet/log | `logger.Info("starting")` |
| One-off setup | Maintenance > value | `cmd/setup` wizard |

### Test lightly (smoke test)

| What | Why | Example in her-go |
|------|-----|-------------------|
| Config loading | Valid YAML parses, invalid doesn't | `config.Load()` |
| Prompt template expansion | Markers resolve, output isn't empty | `ExpandToolSections` |
| External client construction | Client initializes, has right URL | `llm.NewClient()` |

### Test thoroughly (the dam's core)

| What | Why | Example in her-go |
|------|-----|-------------------|
| Fact extraction & storage | Bot's memory — wrong = amnesia or pollution | `memory.SaveFact` |
| Classifier parsing | Wrong parse = garbage saved or valid facts rejected | `parseClassifierResponse` |
| Tool handlers | User-facing actions — wrong = broken commands | `save_fact/handler.go` |
| Compaction math | Wrong = context blowout or lost history | `compact.MaybeCompact` |
| PII scrubbing | Wrong = leaked personal data | `scrub.Scrub` |
| Context building | Wrong = bot loses personality/memory | `tools/context.go` |
| SQL query building | Wrong = data corruption or injection | `DBProxy` queries |
| Token estimation | Wrong = budget miscalculation | `EstimateTokens` |

---

## Table-Driven Tests — The Go Way

Table-driven tests are Go's signature testing pattern. Use them whenever you have
multiple input/output combinations for the same logic.

### Structure

```go
func TestThing(t *testing.T) {
    tests := []struct {
        name string    // subtest name — explains what's being checked
        // inputs
        // expected outputs
    }{
        {
            name: "descriptive case name",
            // ...
        },
    }

    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            // Arrange (if needed beyond struct fields)
            // Act
            // Assert
        })
    }
}
```

### When to Use vs. Not

**Use table-driven** when:
- Same function, different inputs → different outputs
- Testing boundary conditions (empty, nil, max, unicode)
- Parsing/formatting with many valid/invalid cases

**Don't use table-driven** when:
- Each test needs fundamentally different setup
- Test involves complex multi-step interactions
- Only 1-2 cases exist (just write individual tests)

### Good Test Case Names

Names should explain **what breaks**, not what's being tested:

```go
// GOOD — if it fails, you know exactly what's wrong
{"empty string returns zero tokens", "", 0},
{"unicode chars counted by byte length", "🎵", 1},
{"rejects FICTIONAL verdict with reason", "FICTIONAL — game event", ...},

// BAD — failure message is useless
{"test 1", "", 0},
{"unicode", "🎵", 1},
{"fictional", "FICTIONAL — game event", ...},
```

---

## Arrange-Act-Assert (AAA)

Every test follows three sections. Keep Act to **one call**.

```go
func TestEstimateTokens_EmptyString(t *testing.T) {
    // Arrange — set up the scenario
    input := ""

    // Act — do the thing (ONE call)
    got := estimateTokens(input)

    // Assert — check the outcome
    if got != 0 {
        t.Errorf("estimateTokens(%q) = %d, want 0", input, got)
    }
}
```

If Act needs multiple lines, the test is doing too much. Split it.

---

## Error Messages That Help

Go's `t.Errorf` doesn't have `expect().toBe()` magic. Write messages that make
failures self-diagnosing:

```go
// GOOD — shows got, want, and context
t.Errorf("parseClassifierResponse(%q).Type = %q, want %q", input, got.Type, want)

// BAD — tells you nothing useful
t.Errorf("wrong type")
t.Errorf("failed")
```

Pattern: `FunctionName(input) = got, want expected`

Use `t.Fatalf` (not `t.Errorf`) when subsequent assertions would be meaningless
after a failure — e.g., if the result is nil and you're about to access fields.

---

## Mocking Strategy

### The Minimal Mocking Rule

> **"If you're mocking something you wrote, reconsider."**

Mocks replace real behavior with fake behavior. Every mock is a place where your
test diverges from production. Minimize that divergence.

### What to Mock

| Boundary | How to mock | Why |
|----------|-------------|-----|
| LLM API (OpenRouter) | `httptest.NewServer` returning canned JSON | Can't call real LLMs |
| Search APIs (Tavily) | `httptest.NewServer` | External, rate-limited |
| Telegram Bot API | `httptest.NewServer` or interface stub | Can't send real messages |
| Current time | Inject `time.Time` or `func() time.Time` | Deterministic tests |

### What NOT to Mock

| Thing | What to do instead | Why |
|-------|--------------------|-----|
| SQLite | Use real temp DB (`t.TempDir()`) | Mocked DB hides real SQL bugs |
| Our own packages | Use the real package | If it's hard to test with, fix the API |
| Config | Write a real temp YAML file | Config bugs are real bugs |
| File system | Use `t.TempDir()` | Real FS with cleanup is trivial |

### httptest Pattern for External APIs

```go
func TestLLMClient_SendsCorrectRequest(t *testing.T) {
    // Arrange: fake API server
    srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        // Verify request shape
        if r.Header.Get("Authorization") == "" {
            t.Error("missing Authorization header")
        }
        // Return canned response
        json.NewEncoder(w).Encode(map[string]any{
            "choices": []map[string]any{
                {"message": map[string]string{"content": "hello"}},
            },
        })
    }))
    defer srv.Close()

    // Act: call client pointed at fake server
    client := llm.NewClient(srv.URL, "test-key")
    resp, err := client.Chat(context.Background(), messages)

    // Assert
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
    if resp.Content != "hello" {
        t.Errorf("Content = %q, want %q", resp.Content, "hello")
    }
}
```

---

## Bug → Test Pipeline

Every bug is a gift — it tells you where your dam leaks. Turn it into a
permanent guard:

```
1. Bug found       → Understand the conditions that trigger it
2. Write test      → Captures those exact conditions
3. Verify FAILS    → Proves the test would have caught the bug
4. Fix the code    → Make the test pass
5. Commit both     → Test guards that path forever
```

If your fix doesn't come with a test, the bug *will* come back.

---

## Test Quality Signals

### Good Tests

| Property | What it means |
|----------|---------------|
| Behavior-sensitive | Fails when actual functionality breaks |
| Structure-immune | Doesn't break when you refactor safely |
| Deterministic | Same result every time, no flakiness |
| Fast | Gives feedback in seconds |
| Clear diagnosis | When it fails, you know exactly what broke |
| Self-contained | Doesn't depend on external state or ordering |

### Bad Tests (Anti-Patterns)

| Anti-pattern | What's wrong | Fix |
|-------------|--------------|-----|
| Tests implementation | Breaks on safe refactors | Test behavior instead |
| Tests everything | Coverage theater — high number, low value | Apply Confidence Test |
| Mocks everything | Tests prove nothing about real interactions | Use real dependencies |
| Shares state | Tests pass/fail depending on run order | Each test creates own fixtures |
| Sleeps for timing | Flaky, slow | Use channels or polling |
| Ignores errors | `result, _ := DoThing()` | Always check errors in tests |

### The Refactoring Litmus Test

> **If refactoring frequently breaks tests without changing behavior, the tests
> are testing the wrong things.**

Tests should break when *features* break, not when *code moves around*.

---

## Go-Specific Testing Patterns

### t.Helper() — Always Mark Helpers

```go
func assertNoError(t *testing.T, err error) {
    t.Helper() // ← makes error point to caller, not this line
    if err != nil {
        t.Fatalf("unexpected error: %v", err)
    }
}
```

Without `t.Helper()`, failures report the helper's line number instead of the
test's. Always add it to any function that calls `t.Error`/`t.Fatal`.

### t.Cleanup() — Deferred Teardown

```go
func newTestDB(t *testing.T) *sql.DB {
    t.Helper()
    db, err := sql.Open("sqlite3", filepath.Join(t.TempDir(), "test.db"))
    if err != nil {
        t.Fatalf("open db: %v", err)
    }
    t.Cleanup(func() { db.Close() })
    return db
}
```

`t.Cleanup` runs after the test finishes — like `defer` but scoped to the test,
not the function. Useful in helpers where `defer` would close too early.

### Parallel Tests

```go
func TestThing(t *testing.T) {
    t.Parallel() // ← runs concurrently with other parallel tests

    tests := []struct{ name string }{{"a"}, {"b"}}
    for _, tt := range tests {
        t.Run(tt.name, func(t *testing.T) {
            t.Parallel() // ← subtests run concurrently too
            // ...
        })
    }
}
```

Use `t.Parallel()` for tests that don't share state. Skip it for tests that
touch shared databases or files.

### TestMain — Package-Level Setup

```go
func TestMain(m *testing.M) {
    // Setup: create shared fixtures, start servers, etc.
    // Runs once before all tests in the package.

    code := m.Run() // run all tests

    // Teardown: clean up shared resources
    os.Exit(code)
}
```

Use sparingly. Most tests should be self-contained. `TestMain` is for expensive
shared setup (like starting a test server that all tests share).
