---
name: beaver-build
description: >
  Build robust test dams that catch bugs before they flood production.
  The beaver surveys the stream, gathers materials wisely, builds with care,
  reinforces thoroughly, and ships with confidence.
triggers:
  - User asks to "write tests" or "add tests"
  - User says "test this" or "make sure this works"
  - User calls `/beaver-build` or mentions beaver/building dams
  - Deciding what deserves testing
  - Reviewing existing tests for effectiveness
  - Bug needs to become a regression test
  - Asked to "add tests" without specific guidance
  - Evaluating whether tests are providing real value
  - Refactoring causes many tests to break (symptom of bad tests)
---

# beaver-build — Go Test Skill

Build robust test dams. The beaver surveys the stream, gathers materials wisely,
builds with care, reinforces thoroughly, and ships with confidence.

## The Dam — 5-Phase Pipeline

```
SURVEY → GATHER → BUILD → REINFORCE → FORTIFY
   ↓        ↲        ↓          ↲          ↓
Understand Decide   Write       Harden    Ship with
the code   scope    tests       edges     confidence
```

Every testing task flows through these phases. Don't skip SURVEY — building
without understanding creates brittle dams that break on the first flood.

---

## Phase 1: SURVEY — Understand the Stream

Before writing a single `func Test`, answer:

1. **What does this code do for the user?** Not how — what. A tool handler
   saves facts. The compactor trims history. The scrubber protects PII. Start
   from the user-visible behavior.

2. **What breaks if this fails?** A broken classifier lets garbage into the
   memory DB. A broken compactor blows context windows. A broken PII scrubber
   leaks phone numbers. Know the blast radius.

3. **How much confidence do we need?** A utility function that formats time
   needs light coverage. The SQL query builder that talks to the live DB needs
   heavy coverage. Match effort to risk.

4. **What already exists?** Check for existing `_test.go` files in the package.
   Read them. Don't duplicate. Build on what's there.

**Load references:**
- `references/testing-patterns.md` — understand the Testing Trophy and what to test
- `references/hergo-test-infrastructure.md` — know what test utilities exist

---

## Phase 2: GATHER — Decide What Earns a Test

Apply the **Confidence Test**: "Would I notice if this broke in production?"

### Skip (not worth the dam materials)

| What | Why |
|------|-----|
| Trivial getters/setters | `func (c Config) GetModel() string` tests nothing useful |
| Framework behavior | Trust that `cobra` routes commands, `tgbotapi` sends messages |
| Implementation details | Internal struct field layout, private helper ordering |
| One-off scripts | `cmd/setup` runs once — maintenance cost exceeds value |
| Pure logging calls | Trust `charmbracelet/log` works |

### Test lightly (smoke test, happy path)

| What | Why |
|------|-----|
| Config loading | One test that valid YAML parses; don't test every field |
| Prompt assembly | Verify markers expand; don't test every template variation |
| External API clients | Mock at the HTTP boundary, verify request shape |

### Test thoroughly (the dam's core)

| What | Why |
|------|-----|
| Memory operations | Facts are the bot's long-term brain — correctness is critical |
| Tool handlers | Each tool is a user-facing action with observable effects |
| Classifier/parser logic | Wrong parse = garbage in DB or rejected valid facts |
| Compaction algorithm | Wrong math = context window blowout or lost history |
| PII scrubbing | Wrong scrub = leaked personal data or broken responses |
| SQL queries | Wrong query = data corruption, injection vulnerability |
| Context building | Wrong context = bot loses memory, personality breaks |

**Load references:**
- `references/testing-patterns.md` — decision tables for skip vs. test

---

## Phase 3: BUILD — Construct the Dam

### The Go Testing Toolkit

This project uses Go's standard `testing` package. No testify, no gomock, no
frameworks. This is intentional — stdlib tests are readable, portable, and
have zero dependency overhead.

**Core patterns (always use these):**

- **Table-driven tests** for anything with multiple input/output cases
- **`t.Helper()`** on every test helper function (fixes error line reporting)
- **`t.Run()`** subtests for named cases within a table
- **`t.TempDir()`** for throwaway file system fixtures
- **`t.Cleanup()`** for deferred resource teardown
- **`httptest.NewServer()`** for HTTP integration tests
- **Real SQLite** (temp file) for database tests — don't mock the DB

**Write tests following Arrange-Act-Assert:**

```go
func TestParseClassifierResponse_PlainSave(t *testing.T) {
    // Arrange: set up the scenario
    response := "SAVE — real user preference"

    // Act: do the thing (ONE call)
    v := parseClassifierResponse(response)

    // Assert: check the outcome
    if !v.Allowed {
        t.Errorf("Allowed = false, want true")
    }
    if v.Type != "SAVE" {
        t.Errorf("Type = %q, want %q", v.Type, "SAVE")
    }
}
```

The Act section should be **one call**. If it's longer, the test does too much.

**Name tests so they explain what breaks:**

```go
// GOOD — tells you what failed
func TestEstimateTokens_EmptyStringReturnsZero
func TestDBProxy_RejectsUnauthorizedTable
func TestScrubber_PreservesNamesInTier1

// BAD — tells you nothing
func TestTokens
func TestDB
func TestScrub1
```

**One test, one reason to fail.** If a test checks five unrelated things, split it.

### Test Type Decision Tree

```
Is the function pure (input → output, no side effects)?
├─ YES → Table-driven unit test
│        Example: parseClassifierResponse, estimateTokens, FormatTrace
│
└─ NO → Does it touch the database?
   ├─ YES → Integration test with real temp SQLite
   │        Example: fact storage, context building, compaction
   │
   └─ NO → Does it make HTTP calls?
      ├─ YES → httptest.NewServer with controlled responses
      │        Example: LLM client, search API, weather API
      │
      └─ NO → Does it orchestrate multiple things?
         ├─ YES → Integration test with real dependencies
         │        Example: tool handler (needs DB + config)
         │
         └─ NO → Simple unit test
                  Example: config validation, string formatting
```

**Load references:**
- `references/test-templates.md` — concrete templates for each test type
- `references/hergo-test-infrastructure.md` — existing helpers to import

---

## Phase 4: REINFORCE — Harden the Dam

### Mock only at external boundaries

If you're mocking something we wrote, reconsider. Mock at trust boundaries:

| Mock this | Why |
|-----------|-----|
| OpenRouter API responses | Can't call real LLMs in tests |
| Tavily/Open Library responses | External service, rate-limited |
| Telegram bot API | Can't send real messages |
| File system (sometimes) | Use `t.TempDir()` for real FS when possible |

| DON'T mock this | Why |
|-----------------|-----|
| SQLite database | Use a real temp DB — that's where bugs hide |
| Our own packages | If `memory.Store` is hard to test with, fix the API |
| Config loading | Use a real temp YAML file |

### Turn every bug into a regression test

The bug-to-test pipeline:

1. Bug reported (or discovered)
2. Reproduce: write a test that **captures the bug's conditions**
3. Verify: test **fails** (proves it catches the bug)
4. Fix: change the code
5. Verify: test **passes** (proves the fix works)
6. The test now guards that code path forever

### Edge cases worth checking

- **Empty/nil inputs**: Empty string, nil slice, zero struct
- **Boundary values**: Max tokens, empty history, single message
- **Unicode/special chars**: Emoji in facts, HTML in traces, newlines in prompts
- **Concurrent access**: If the code uses goroutines, test with `-race`
- **Error paths**: DB connection fails, API returns 500, malformed JSON

### Keep tests co-located

Tests live in the same package as the code they test:

```
memory/
├── store.go
├── store_test.go      ← right here, same package
├── context.go
└── context_test.go    ← right here, same package
```

This gives tests access to unexported functions (white-box testing), which is
appropriate for unit tests. For black-box integration tests, use `package memory_test`
(note the `_test` suffix) to test only the public API.

---

## Phase 5: FORTIFY — Ship with Confidence

### Mandatory verification before shipping

```bash
# Run the package's tests
go test ./path/to/package/...

# Run ALL tests (do this before any PR)
go test ./...

# Run with race detector (catches concurrent bugs)
go test -race ./...

# Run vet (catches subtle bugs the compiler misses)
go vet ./...
```

### Self-review checklist

Before marking tests as done, verify:

```
[ ] Tests describe behavior, not implementation details
[ ] Each test has one clear reason to fail
[ ] Test names explain what breaks when they fail
[ ] Table-driven tests used for multiple input/output cases
[ ] t.Helper() called in every helper function
[ ] Mocks limited to external boundaries (APIs, not our own code)
[ ] Real SQLite used for database tests (not mocked)
[ ] Bug fixes include regression tests
[ ] Tests run fast (seconds, not minutes)
[ ] Error paths tested, not just happy paths
[ ] go test -race passes
[ ] go vet passes
```

### Coverage check (optional, after tests pass)

```bash
# Generate coverage report
go test -coverprofile=coverage.out ./...

# View in browser
go tool cover -html=coverage.out
```

Coverage is a **signal**, not a target. 80% coverage with good tests beats 100%
coverage with garbage tests. Don't chase the number — chase confidence.

---

## Reference Routing

Each phase loads specific references. Don't read everything up front — load what
you need when you need it.

| Phase | Load | Why |
|-------|------|-----|
| SURVEY | `testing-patterns.md` | Understand Trophy shape, what matters |
| SURVEY | `hergo-test-infrastructure.md` | Know what helpers exist |
| GATHER | `testing-patterns.md` | Decision tables for skip vs. test |
| BUILD | `test-templates.md` | Concrete Go templates to follow |
| BUILD | `hergo-test-infrastructure.md` | Import paths for existing helpers |
| REINFORCE | `testing-patterns.md` | Mocking rules, bug-to-test pipeline |
| FORTIFY | Self-review checklist above | Quality gate |
