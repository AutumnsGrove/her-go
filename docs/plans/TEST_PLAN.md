# her-go Test Buildout Plan

A phased testing strategy using the beaver-build methodology. Each package is categorized by risk, assigned a test type, and prioritized.

---

## Testing Philosophy

**The Confidence Test:** Before writing any test, ask — "Would I notice if this broke in production?"

- **YES** → test thoroughly (multiple cases, error paths, edge cases)
- **MAYBE** → test lightly (happy path only)
- **NO** → skip it

**The Testing Trophy (Go edition):**
```
                    E2E (few)
              Integration (many) ← confidence lives here
                Unit (some)
        Static Analysis (always on)
```

**Rules:**
- Go stdlib only — no testify, no gomock
- Mock at external boundaries only (APIs, not our own packages)
- Real SQLite with `t.TempDir()` for database tests
- `httptest.NewServer()` for HTTP client tests
- Table-driven tests for multiple input/output cases
- Every bug becomes a regression test

---

## Current Coverage

16 test files exist, mostly in the skills/tools subsystem:

| Package | Files | What's Covered |
|---------|-------|---------------|
| `agent/` | `classifier_test.go`, `prompt_test.go` | Classifier parsing (8 cases), prompt marker replacement |
| `compact/` | `compact_test.go` | Token estimation, compaction logic (14 cases) |
| `skills/loader/` | 5 test files | DB proxy, HTTP proxy, registry, sidecar, trust |
| `skills/skillkit/go/` | 4 test files | Args, DB, HTTP, output |
| `tools/` | `loader_test.go`, `render_test.go`, `trace_test.go` | YAML loading, rendering, trace specs |

**What's missing:** The entire core — memory store, tool handlers, agent loop, scheduler, config, scrub, LLM client, embed client.

---

## Phase 1: Foundation (Memory + Core Logic)

These are the highest-risk packages. If they break, the bot is brain-damaged.

### 1.1 `memory/store.go` — The Brain

**Risk:** Critical. Every conversation, fact, and metric flows through here.
**Test type:** Integration (real temp SQLite)
**Priority:** P0

Tests needed:
- [ ] `TestStore_Init` — Schema creation on fresh DB, WAL mode enabled
- [ ] `TestStore_SaveMessage` — Insert + retrieve, raw vs scrubbed content
- [ ] `TestStore_SaveFact` — Basic save, duplicate detection via embedding similarity
- [ ] `TestStore_UpdateFact` — Modify existing fact, verify old content gone
- [ ] `TestStore_RemoveFact` — Soft/hard delete behavior
- [ ] `TestStore_GetContextFacts` — KNN semantic search returns relevant facts
- [ ] `TestStore_ZettelkastenLinking` — Auto-link creates 1-hop neighbor relationships
- [ ] `TestStore_SaveSummary` — Compaction summary persistence
- [ ] `TestStore_LatestSummary` — Retrieves most recent summary for conversation
- [ ] `TestStore_ScheduledTasks` — CRUD for reminders/cron jobs
- [ ] `TestStore_MoodEntries` — Insert + query mood ratings/tags
- [ ] `TestStore_Expenses` — Receipt + line item storage
- [ ] `TestStore_PIIVault` — Token ↔ value round-trip
- [ ] `TestStore_Metrics` — Token count / cost recording
- [ ] `TestStore_AgentTurns` — Audit trail persistence
- [ ] `TestStore_PendingConfirmations` — Confirmation flow lifecycle

**Helpers to build:**
- `testStore(t *testing.T) *Store` — creates a temp DB, runs migrations, returns Store, auto-cleanup
- `seedFacts(t *testing.T, s *Store, facts []Fact)` — bulk insert for context tests

**Note:** The embed client dependency (for vector search) will need a stub — this is one of the few places where we mock our own code, because the embedding model is an external HTTP service.

### 1.2 `memory/context.go` — Context Assembly

**Risk:** High. Bad context = bad replies.
**Test type:** Integration (real DB with seeded facts)
**Priority:** P0

Tests needed:
- [ ] `TestBuildContext_NoFacts` — Empty DB returns empty/minimal context
- [ ] `TestBuildContext_RelevantFacts` — Seeded facts appear in context
- [ ] `TestBuildContext_Ordering` — Most relevant facts appear first
- [ ] `TestBuildContext_TokenBudget` — Context respects token limits

### 1.3 `memory/extract.go` — Fact Extraction

**Risk:** High. Bad extraction = lost memories.
**Test type:** Unit (prompt template testing, not LLM calls)
**Priority:** P1

Tests needed:
- [ ] `TestExtractionPrompt` — Template renders correctly with conversation history
- [ ] `TestParseExtractedFacts` — JSON response → Fact structs

### 1.4 `compact/compact.go` — Already Has Tests, Expand

**Risk:** Medium-high. Bad compaction = lost context or token blowout.
**Test type:** Unit + Integration
**Priority:** P1

Additional tests needed:
- [ ] `TestCompact_WithRealSummary` — Integration with memory store summary persistence
- [ ] `TestCompact_BoundaryConditions` — Exactly at 75% threshold, one message over
- [ ] `TestCompact_EmptyHistory` — No messages to compact

---

## Phase 2: Agent & Tools (The Decision Engine)

### 2.1 `agent/classifier.go` — Already Has Tests, Expand

**Risk:** High. Classifier failures let garbage into memory.
**Test type:** Unit (response parsing)
**Priority:** P1

Additional tests needed:
- [ ] `TestClassifier_RewriteSuggestions` — Verdict includes rewrite text
- [ ] `TestClassifier_RetryBudget` — Respects max retries for repeated saves
- [ ] `TestClassifier_MalformedResponse` — Graceful handling of garbled LLM output
- [ ] `TestClassifier_FailOpen` — When classifier is down, writes proceed

### 2.2 `tools/` — Individual Tool Handlers

**Risk:** High. Tools are user-facing actions.
**Test type:** Unit (pure logic) + Integration (DB-touching tools)
**Priority:** P1

Each tool handler needs at minimum a happy-path test. High-priority tools:

#### Memory Tools (touch the DB)
- [ ] `TestSaveFactHandler` — Calls classifier, writes to DB on ACCEPT
- [ ] `TestSaveFactHandler_Rejected` — Classifier rejects fictional content
- [ ] `TestUpdateFactHandler` — Modifies existing fact
- [ ] `TestRemoveFactHandler` — Deletes fact by ID
- [ ] `TestRecallMemoriesHandler` — Returns relevant facts for query
- [ ] `TestSearchHistoryHandler` — Searches message history

#### Communication Tools (pure logic)
- [ ] `TestReplyHandler` — Formats message, respects length limits
- [ ] `TestThinkHandler` — Returns structured thinking output
- [ ] `TestDoneHandler` — Signals loop termination
- [ ] `TestNoActionHandler` — Returns without side effects

#### Planning Tools (DB + logic)
- [ ] `TestCreateScheduleHandler` — Persists task to DB
- [ ] `TestUpdateScheduleHandler` — Modifies existing task
- [ ] `TestDeleteScheduleHandler` — Removes task
- [ ] `TestListSchedulesHandler` — Returns formatted list
- [ ] `TestCreateReminderHandler` — One-shot reminder creation

#### Finance Tools (DB + external)
- [ ] `TestScanReceiptHandler` — OCR result → expense record
- [ ] `TestQueryExpensesHandler` — Date range / category queries
- [ ] `TestUpdateExpenseHandler` — Modify expense metadata
- [ ] `TestDeleteExpenseHandler` — Remove expense record

#### Context Tools (mixed)
- [ ] `TestGetCurrentTimeHandler` — Returns formatted time
- [ ] `TestViewImageHandler` — Vision API call (httptest mock)
- [ ] `TestSetLocationHandler` — Updates config location

#### Persona Tools (DB)
- [ ] `TestUpdatePersonaHandler` — Writes new persona version
- [ ] `TestSaveSelfFactHandler` — Bot's self-knowledge storage

#### Skill Tools (integration)
- [ ] `TestFindSkillHandler` — Searches skill registry
- [ ] `TestRunSkillHandler` — Executes skill in sandbox

**Helper to build:**
- `testToolContext(t *testing.T) *tools.Context` — creates a Context with real temp DB and stubbed external services

### 2.3 `agent/agent.go` — The Orchestrator

**Risk:** Critical, but hard to test in isolation.
**Test type:** Integration (needs careful setup)
**Priority:** P2 (after tools + memory are tested)

Tests needed:
- [ ] `TestAgent_SingleToolCall` — Agent calls one tool and returns
- [ ] `TestAgent_MultipleToolCalls` — Agent chains tool calls
- [ ] `TestAgent_DoneTerminates` — Loop ends when `done` tool called
- [ ] `TestAgent_MaxIterations` — Loop stops after limit
- [ ] `TestAgent_ToolError` — Graceful handling of tool failure

**Challenge:** This requires mocking the LLM client to return controlled tool call sequences. This is the one place where a mock LLM is justified — it's an external boundary.

**Helper to build:**
- `mockLLMClient(responses []ChatResponse)` — returns canned responses in sequence

---

## Phase 3: External Clients & Services

### 3.1 `llm/client.go` — LLM API Client

**Risk:** Medium. Well-tested by usage, but edge cases matter.
**Test type:** Integration (httptest)
**Priority:** P2

Tests needed:
- [ ] `TestLLMClient_ChatCompletion` — Basic request/response
- [ ] `TestLLMClient_ToolCalling` — Tool call in response
- [ ] `TestLLMClient_StreamingToolCalls` — Multiple tool calls in one response
- [ ] `TestLLMClient_RateLimit` — 429 triggers fallback model retry
- [ ] `TestLLMClient_Timeout` — Context cancellation
- [ ] `TestLLMClient_MalformedJSON` — Graceful error on bad response
- [ ] `TestLLMClient_ContentParts` — Multi-modal message marshaling

### 3.2 `embed/embed.go` — Embedding Client

**Risk:** Medium. Incorrect embeddings = bad memory retrieval.
**Test type:** Unit (cosine similarity) + Integration (httptest for API)
**Priority:** P2

Tests needed:
- [ ] `TestCosineSimilarity` — Math correctness, edge cases (zero vector, identical)
- [ ] `TestEmbed_APICall` — Request shape, response parsing
- [ ] `TestEmbed_DimensionConfig` — Correct dimensions per provider

### 3.3 `search/tavily.go` — Web Search Client

**Risk:** Low-medium.
**Test type:** Integration (httptest)
**Priority:** P3

Tests needed:
- [ ] `TestTavilySearch` — Request params, response parsing
- [ ] `TestTavilyExtract` — URL content extraction
- [ ] `TestTavilySearch_Error` — API error handling

### 3.4 `search/books.go` — Book Search

**Risk:** Low.
**Test type:** Integration (httptest)
**Priority:** P3

Tests needed:
- [ ] `TestBookSearch` — Query → results parsing

### 3.5 `weather/weather.go` — Weather Client

**Risk:** Low.
**Test type:** Integration (httptest)
**Priority:** P3

Tests needed:
- [ ] `TestWeatherForecast` — Coordinates → weather data
- [ ] `TestWeatherGeocoding` — Location name → coordinates
- [ ] `TestWeatherCaching` — TTL cache prevents redundant calls

---

## Phase 4: Bot, Scheduler & Infrastructure

### 4.1 `scheduler/` — Task Execution

**Risk:** Medium-high. Missed reminders = user frustration.
**Test type:** Integration (real DB)
**Priority:** P2

Tests needed:
- [ ] `TestScheduler_TickProcessing` — Due tasks get executed
- [ ] `TestScheduler_CronEvaluation` — Cron expressions fire correctly
- [ ] `TestScheduler_QuietHours` — Tasks deferred during quiet hours
- [ ] `TestScheduler_RateLimit` — Max tasks per day respected
- [ ] `TestScheduler_BusyCheck` — Skips when agent is busy
- [ ] `TestScheduler_OneShot` — One-time tasks removed after execution
- [ ] `TestScheduler_Priority` — Critical tasks bypass rate limits

### 4.2 `config/config.go` — Configuration

**Risk:** Medium. Bad config = nothing works.
**Test type:** Unit (file parsing)
**Priority:** P2

Tests needed:
- [ ] `TestConfig_LoadDefaults` — Example config parses without error
- [ ] `TestConfig_EnvExpansion` — `${VAR}` replaced correctly
- [ ] `TestConfig_MissingFile` — Graceful error
- [ ] `TestConfig_PartialOverride` — User config merges over defaults
- [ ] `TestConfig_SetTrace` — Surgical YAML edit for trace toggle
- [ ] `TestConfig_SetLocation` — Surgical YAML edit for location

### 4.3 `scrub/` — PII Scrubbing

**Risk:** High (security-critical).
**Test type:** Unit (pattern matching)
**Priority:** P1

Tests needed:
- [ ] `TestScrub_SSN` — Hard identifiers fully redacted
- [ ] `TestScrub_CreditCard` — Card numbers redacted
- [ ] `TestScrub_PhoneNumber` — Phone tokenized (reversible)
- [ ] `TestScrub_Email` — Email tokenized (reversible)
- [ ] `TestScrub_NoFalsePositives` — Normal text passes through unchanged
- [ ] `TestScrub_Deanonymize` — Tokens replaced with originals in replies
- [ ] `TestScrub_Unicode` — Non-ASCII content handled correctly
- [ ] `TestScrub_MixedContent` — Text with multiple PII types

### 4.4 `bot/` — Telegram Handlers

**Risk:** Medium, but heavily dependent on telebot library.
**Test type:** Limited unit tests on helper functions
**Priority:** P3

Tests needed:
- [ ] `TestPaginate` — Long messages split correctly
- [ ] `TestHelpers` — Any pure utility functions

### 4.5 `persona/evolution.go` — Persona Evolution

**Risk:** Low-medium.
**Test type:** Unit
**Priority:** P3

Tests needed:
- [ ] `TestTraitTracking` — Trait shift detection
- [ ] `TestEvolutionTrigger` — Reflection threshold logic

### 4.6 `tui/` — Event System

**Risk:** Low (display only).
**Test type:** Unit
**Priority:** P3

Tests needed:
- [ ] `TestEventBus_PubSub` — Events delivered to subscribers
- [ ] `TestEventTypes` — Each event type satisfies Event interface

---

## Phase 5: Layer System & Prompt Assembly

### 5.1 `agent/layers/` — Prompt Layers

**Risk:** Medium. Bad layers = bad prompts = bad behavior.
**Test type:** Unit
**Priority:** P2

Tests needed:
- [ ] `TestLayerRegistry` — All layers register correctly
- [ ] `TestLayer_ChatVsAgent` — Stream filtering works
- [ ] `TestLayer_HotReload` — File-backed layers pick up changes
- [ ] `TestLayer_Ordering` — Layers assembled in correct order
- [ ] Individual layer render tests for complex layers (facts, history, tools)

---

## Test Infrastructure To Build First

Before writing any package tests, these shared helpers need to exist:

### `testutil/` package (new)

```go
// testutil/db.go
func TempStore(t *testing.T) *memory.Store
// Creates temp SQLite, runs migrations, registers cleanup.

// testutil/embed.go  
func StubEmbedClient(t *testing.T) *embed.Client
// Returns a client that produces deterministic embeddings
// (e.g., hash-based vectors for reproducible similarity).

// testutil/llm.go
func MockLLMServer(t *testing.T, responses ...llm.ChatResponse) *httptest.Server
// Returns an httptest server that serves canned LLM responses in order.

// testutil/tools.go
func TestToolContext(t *testing.T) *tools.Context
// Creates a Context with real temp DB, stub embed, stub LLM.
```

This keeps individual test files focused on behavior, not setup boilerplate.

---

## Execution Order

```
Step 1: Build testutil/ helpers (TempStore, StubEmbed, MockLLM)
Step 2: Phase 1 — memory/store_test.go, memory/context_test.go
Step 3: Phase 2 — scrub tests, then tool handler tests
Step 4: Phase 2 — agent classifier expansion, agent loop tests
Step 5: Phase 3 — LLM client, embed client, config
Step 6: Phase 4 — scheduler, layers
Step 7: Phase 3+4 — remaining low-priority packages
```

Each step should end with `go test -race ./...` passing and `go vet ./...` clean.

---

## Beaver-Build Checklist (apply to every test file)

```
[ ] Tests describe behavior, not implementation details
[ ] Each test has one clear reason to fail
[ ] Test names explain what breaks when they fail
[ ] Table-driven tests used for multiple input/output cases
[ ] t.Helper() called in every helper function
[ ] Mocks limited to external boundaries (APIs, not our code)
[ ] Real SQLite used for database tests (not mocked)
[ ] Bug fixes include regression tests
[ ] Tests run fast (seconds, not minutes)
[ ] Error paths tested, not just happy paths
[ ] go test -race passes
[ ] go vet passes
```

---

## Metrics / Goals

- **Phase 1 target:** Memory store has 15+ test cases, all CRUD operations covered
- **Phase 2 target:** Every tool handler has at least 1 happy-path test
- **Phase 3 target:** All HTTP clients have httptest-based tests
- **Full buildout:** `go test ./...` runs in under 30 seconds, covers all packages with .go files
- **Ongoing:** Every bug fix ships with a regression test
