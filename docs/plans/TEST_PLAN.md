# her-go Test Buildout Plan

A phased testing strategy using the beaver-build methodology. Each package is categorized by risk, assigned a test type, and prioritized.

**Last updated:** 2026-04-22

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

## Current Coverage (as of 2026-04-22)

**20 packages passing, 32 packages with no test files, 51 test files total.**

All tests pass with `-race`.

| Package | Test Files | What's Covered |
|---------|-----------|----------------|
| `agent/` | 4 files | Integration (basic turn, tool failure, deferred search load), continuation summary, memory agent, prompt markers |
| `bot/` | 1 file | Mood wizard |
| `calendar/` | 2 files | Bridge + fake bridge (insert, list, filter, update, delete) |
| `classifier/` | 1 file | Response parsing (10 cases), rewrite extraction, rejection messages, Check() with mock LLM |
| `compact/` | 1 file | Token estimation, compaction threshold logic (8 cases) |
| `config/` | 1 file | SetLocation (6 cases, idempotent, missing file), FormatFloat, MatchJob (11 cases) |
| `embed/` | 1 file | IsAvailable (up, down, server error) |
| `layers/` | 1 file | Chat mood layer (8 cases: empty, single, inject, source tag, rollup, humanTime) |
| `llm/` | 1 file | Streaming (single tool call, batched abort, truncated JSON, token delivery, stream flag) |
| `memory/` | 4 files | Calendar events CRUD, inbox send/consume, mood entries (15+ cases), scheduler tasks |
| `mood/` | 8 files | Agent, graph, prompt, proposal, rollup task, signals, sweeper, vocab |
| `scheduler/` | 4 files | Loader (config parsing, cron computation), registry (upsert, kind mismatch, cron changes), retry, runner |
| `search/` | 1 file | Book search |
| `tools/` (shared) | 6 files | Dispatch (unknown tool, malformed JSON), YAML loader, style/length gates, render (hot tools, categories), shift notes, trace |
| `tools/remove_memory/` | 1 file | Handler test |
| `tools/reply/` | 1 file | Style test |
| `tools/send_task/` | 1 file | Handler test |
| `trace/` | 3 files | Board, advanced board, registry |
| `turn/` | 2 files | Registry, tracker |
| `weather/` | 1 file | Weather tests |

**Archived (in `_junkdrawer/`, excluded from build):**
- `skills/loader/` — 6 test files
- `skills/skillkit/go/` — 4 test files

---

## Phase 1: Foundation (Memory + Core Logic)

These are the highest-risk packages. If they break, the bot is brain-damaged.

### 1.1 `memory/store.go` — The Brain

**Risk:** Critical. Every conversation, fact, and metric flows through here.
**Test type:** Integration (real temp SQLite)
**Priority:** P0
**Status:** Partially covered — calendar, inbox, mood, scheduler store tests exist. Core CRUD is untested.

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
- [ ] `TestStore_PIIVault` — Token ↔ value round-trip
- [ ] `TestStore_Metrics` — Token count / cost recording
- [ ] `TestStore_AgentTurns` — Audit trail persistence
- [ ] `TestStore_PendingConfirmations` — Confirmation flow lifecycle
- [x] `TestStore_ScheduledTasks` — CRUD for reminders/cron jobs *(store_scheduler_test.go)*
- [x] `TestStore_MoodEntries` — Insert + query mood ratings/tags *(store_mood_test.go, 15+ cases)*
- [x] `TestStore_CalendarEvents` — CRUD + filters *(store_calendar_test.go)*
- [x] `TestStore_Inbox` — Send + consume lifecycle *(store_inbox_test.go)*

**Helpers to build:**
- `testStore(t *testing.T) *Store` — creates a temp DB, runs migrations, returns Store, auto-cleanup
  *(Note: individual store test files may already have local helpers — consolidate if so)*

**Note:** The embed client dependency (for vector search) will need a stub — this is one of the few places where we mock our own code, because the embedding model is an external HTTP service.

### 1.2 `memory/context.go` — Context Assembly

**Risk:** High. Bad context = bad replies.
**Test type:** Integration (real DB with seeded facts)
**Priority:** P0
**Status:** Not tested.

Tests needed:
- [ ] `TestBuildContext_NoFacts` — Empty DB returns empty/minimal context
- [ ] `TestBuildContext_RelevantFacts` — Seeded facts appear in context
- [ ] `TestBuildContext_Ordering` — Most relevant facts appear first
- [ ] `TestBuildContext_TokenBudget` — Context respects token limits

### 1.3 `memory/extract.go` — Fact Extraction

**Risk:** High. Bad extraction = lost memories.
**Test type:** Unit (prompt template testing, not LLM calls)
**Priority:** P1
**Status:** Not tested.

Tests needed:
- [ ] `TestExtractionPrompt` — Template renders correctly with conversation history
- [ ] `TestParseExtractedFacts` — JSON response → Fact structs

### 1.4 `compact/compact.go` — Compaction

**Risk:** Medium-high. Bad compaction = lost context or token blowout.
**Test type:** Unit + Integration
**Priority:** P1
**Status:** Core logic covered (8 cases). Expansion items remain.

Existing coverage:
- [x] `TestEstimateTokens` / `TestEstimateHistoryTokens` — Token math
- [x] `TestMaybeCompact_UnderThreshold` / `OverThreshold` / `ZeroBudget` — Threshold logic
- [x] `TestMaybeCompact_RealisticSimMessages` — Realistic workload
- [x] `TestMaybeCompact_OnlyCountsUnsummarized` — Summary exclusion

Additional tests:
- [ ] `TestCompact_WithRealSummary` — Integration with memory store summary persistence
- [ ] `TestCompact_BoundaryConditions` — Exactly at threshold, one message over
- [ ] `TestCompact_EmptyHistory` — No messages to compact

---

## Phase 2: Security & Quality Gates

### 2.1 `scrub/` — PII Scrubbing

**Risk:** High (security-critical). This is the privacy layer.
**Test type:** Unit (pattern matching)
**Priority:** P0
**Status:** No tests.

Tests needed:
- [ ] `TestScrub_SSN` — Hard identifiers fully redacted
- [ ] `TestScrub_CreditCard` — Card numbers redacted
- [ ] `TestScrub_PhoneNumber` — Phone tokenized (reversible)
- [ ] `TestScrub_Email` — Email tokenized (reversible)
- [ ] `TestScrub_NoFalsePositives` — Normal text passes through unchanged
- [ ] `TestScrub_Deanonymize` — Tokens replaced with originals in replies
- [ ] `TestScrub_Unicode` — Non-ASCII content handled correctly
- [ ] `TestScrub_MixedContent` — Text with multiple PII types

### 2.2 `classifier/` — Memory + Reply Quality Gate

**Risk:** High. Classifier failures let garbage into memory.
**Test type:** Unit (response parsing) + Integration (mock LLM)
**Priority:** P1
**Status:** Well covered. Core parsing, rewrite extraction, Check() with mock LLM all tested.

Existing coverage:
- [x] `TestParseResponse` — 10 cases (SAVE, FICTIONAL, INFERRED, LOW_VALUE, STYLE_ISSUE, PASS, unparseable, multiline)
- [x] `TestExtractRewrite` — Quoted, unquoted, missing, mixed case
- [x] `TestRejectionMessage` — Soft verdict with rewrite, unknown verdict fallback
- [x] `TestCheck` — Nil client fails open, unknown writeType fails open, SAVE/LOW_VALUE/PASS, snippet context, server error

Additional tests (nice to have):
- [ ] `TestCheck_RetryBudget` — Respects max retries for repeated saves
- [ ] `TestCheck_Timeout` — Context cancellation mid-check

---

## Phase 3: Agent & Tools (The Decision Engine)

### 3.1 `agent/` — The Orchestrator

**Risk:** Critical, but now has good integration tests.
**Test type:** Integration (mock LLM)
**Priority:** P1
**Status:** Core loop covered. Memory agent covered.

Existing coverage:
- [x] `TestRun_BasicTurn` — Agent calls tool and returns
- [x] `TestRun_ToolFailureTurn` — Graceful handling of tool failure
- [x] `TestRun_ContinuationTurn` — Multi-turn continuation
- [x] `TestRun_DeferredSearchLoad` — Deferred tool loading
- [x] `TestBuildContinuationSummary` — HTML stripping, entity unescaping, truncation (5 cases)
- [x] `TestRunMemoryAgent_SavesMemoryAndCallsDone` — Memory agent happy path
- [x] `TestRunMemoryAgent_NilLLM` — Nil guard
- [x] `TestReplaceBetweenMarkers` — Prompt assembly (5 cases)
- [x] `TestExpandToolSections` — Tool section expansion

Additional tests:
- [ ] `TestAgent_MaxIterations` — Loop stops after limit
- [ ] `TestAgent_MultipleToolCalls` — Agent chains multiple tool calls in one turn

### 3.2 `tools/` — Individual Tool Handlers

**Risk:** High. Tools are user-facing actions.
**Test type:** Unit (pure logic) + Integration (DB-touching tools)
**Priority:** P1
**Status:** 3 of 26 tool handler directories have tests. Shared tools package well covered.

Shared tools coverage (already done):
- [x] Dispatch — unknown tool, malformed JSON
- [x] YAML loader
- [x] Style gate + length gate (memory quality)
- [x] Render — hot tools list, category table, hints completeness
- [x] Shift notes — parse + serialize (8+ cases)
- [x] Trace specs

Per-tool handler coverage:
- [x] `remove_memory/` — handler test
- [x] `reply/` — style test
- [x] `send_task/` — handler test

**Uncovered tool handlers (23):**
Each needs at minimum a happy-path test. Grouped by priority:

#### P1 — Memory tools (touch the DB, high risk)
- [ ] `save_memory/` — Calls classifier, writes to DB on ACCEPT
- [ ] `save_self_memory/` — Bot's self-knowledge storage
- [ ] `update_memory/` — Modifies existing fact
- [ ] `recall_memories/` — Returns relevant facts for query
- [ ] `split_memory/` — Splits compound facts

#### P1 — Communication tools (pure logic, quick wins)
- [ ] `think/` — Returns structured thinking output
- [ ] `done/` — Signals loop termination
- [ ] `notify_agent/` — Cross-agent notification

#### P2 — Calendar tools (DB + logic)
- [ ] `calendar_create/` — Creates calendar event
- [ ] `calendar_update/` — Modifies event
- [ ] `calendar_delete/` — Removes event
- [ ] `calendar_list/` — Lists events with filters
- [ ] `list_calendars/` — Lists available calendars
- [ ] `shift_hours/` — Shift time tracking

#### P2 — Context & utility tools
- [ ] `get_time/` — Returns formatted time
- [ ] `get_weather/` — Weather lookup
- [ ] `set_location/` — Updates config location
- [ ] `view_image/` — Vision API call (httptest mock)
- [ ] `use_tools/` — Meta tool for tool selection

#### P2 — Search & web tools
- [ ] `web_search/` — Web search via Tavily
- [ ] `web_read/` — URL content extraction
- [ ] `search_books/` — Book search

#### P3 — Persona tools
- [ ] `update_persona/` — Writes new persona version

---

## Phase 4: External Clients & Services

### 4.1 `llm/client.go` — LLM API Client

**Risk:** Medium. Core streaming is tested, but edge cases matter.
**Test type:** Integration (httptest)
**Priority:** P2
**Status:** Streaming well covered (6 cases). Non-streaming and error paths remain.

Existing coverage:
- [x] `TestDoStreamRequest_SingleToolCall`
- [x] `TestDoStreamRequest_AbortsOnBatchedToolCalls`
- [x] `TestDoStreamRequest_TruncatedJSON`
- [x] `TestDoStreamingChat_TokensDelivered`
- [x] `TestChatCompletionWithTools_SendsStreamTrue`
- [x] `TestChatCompletion_NoStreamField`

Additional tests:
- [ ] `TestLLMClient_RateLimit` — 429 triggers fallback model retry
- [ ] `TestLLMClient_Timeout` — Context cancellation
- [ ] `TestLLMClient_MalformedJSON` — Graceful error on bad response body
- [ ] `TestLLMClient_ContentParts` — Multi-modal message marshaling

### 4.2 `embed/embed.go` — Embedding Client

**Risk:** Medium. Incorrect embeddings = bad memory retrieval.
**Test type:** Unit (cosine similarity) + Integration (httptest for API)
**Priority:** P2
**Status:** Only availability checks tested.

Existing coverage:
- [x] `TestIsAvailable_Up` / `Down` / `ServerError`

Additional tests:
- [ ] `TestCosineSimilarity` — Math correctness, edge cases (zero vector, identical)
- [ ] `TestEmbed_APICall` — Request shape, response parsing
- [ ] `TestEmbed_DimensionConfig` — Correct dimensions per provider

### 4.3 `search/` — Search Clients

**Risk:** Low-medium.
**Test type:** Integration (httptest)
**Priority:** P3
**Status:** Book search tested. Tavily untested.

- [x] `TestBookSearch` — Query → results parsing

Additional tests:
- [ ] `TestTavilySearch` — Request params, response parsing
- [ ] `TestTavilyExtract` — URL content extraction
- [ ] `TestTavilySearch_Error` — API error handling

### 4.4 `weather/weather.go` — Weather Client

**Risk:** Low.
**Test type:** Integration (httptest)
**Priority:** P3
**Status:** Has tests.

- [x] Weather tests exist

---

## Phase 5: Infrastructure & Supporting Systems

### 5.1 `scheduler/` — Task Execution

**Risk:** Medium-high. Missed reminders = user frustration.
**Test type:** Integration (real DB)
**Priority:** P2
**Status:** Well covered (4 test files, 15+ cases).

Existing coverage:
- [x] Loader — Config parsing, cron next-fire computation, invalid cron errors
- [x] Registry — Upsert, kind mismatch, invalid backoff, cron change detection, handler skip
- [x] Retry — retry logic
- [x] Runner — execution logic

Additional tests:
- [ ] `TestScheduler_QuietHours` — Tasks deferred during quiet hours
- [ ] `TestScheduler_RateLimit` — Max tasks per day respected
- [ ] `TestScheduler_BusyCheck` — Skips when agent is busy

### 5.2 `config/config.go` — Configuration

**Risk:** Medium. Bad config = nothing works.
**Test type:** Unit (file parsing)
**Priority:** P2
**Status:** SetLocation and MatchJob well covered. Core loading untested.

Existing coverage:
- [x] `TestSetLocation` — 6 cases (update, partial, append, comments, empty name, float formatting)
- [x] `TestSetLocation_IdempotentUpdate` / `MissingFile`
- [x] `TestFormatFloat`
- [x] `TestMatchJob` — 11 cases (exact, case insensitive, aliases, no match)

Additional tests:
- [ ] `TestConfig_LoadDefaults` — Example config parses without error
- [ ] `TestConfig_EnvExpansion` — `${VAR}` replaced correctly
- [ ] `TestConfig_PartialOverride` — User config merges over defaults

### 5.3 `layers/` — Prompt Layers

**Risk:** Medium. Bad layers = bad prompts = bad behavior.
**Test type:** Unit
**Priority:** P2
**Status:** Chat mood layer covered (8 cases). Others untested.

Existing coverage:
- [x] `TestBuildChatMood_*` — 7 cases (empty, single, inject, source tag, rollup recency, detail count)
- [x] `TestHumanTime`

Additional tests:
- [ ] `TestLayerRegistry` — All layers register correctly
- [ ] `TestLayer_Ordering` — Layers assembled in correct order
- [ ] Individual layer render tests for complex layers (facts, history, tools)

### 5.4 `mood/` — Mood System

**Risk:** Medium.
**Test type:** Unit + Integration
**Priority:** Done (maintenance only)
**Status:** Thoroughly covered (8 test files).

- [x] Agent, graph, prompt, proposal, rollup task, signals, sweeper, vocab — all tested

### 5.5 `trace/` — Thinking Traces

**Risk:** Low-medium (display only).
**Test type:** Unit
**Priority:** Done (maintenance only)
**Status:** Covered (3 test files).

- [x] Board, advanced board, registry — all tested

### 5.6 `turn/` — Turn Tracking

**Risk:** Low-medium.
**Test type:** Unit
**Priority:** Done (maintenance only)
**Status:** Covered (2 test files).

- [x] Registry, tracker — all tested

### 5.7 Other packages

| Package | Risk | Status | Notes |
|---------|------|--------|-------|
| `bot/` | Medium | 1 test (mood wizard) | Heavily depends on telebot. Test helpers/pure functions only. |
| `persona/` | Low-medium | No tests | Trait tracking, evolution trigger |
| `tui/` | Low (display) | No tests | Event bus pub/sub |
| `vision/` | Low | No tests | Thin wrapper around API |
| `voice/` | Low | No tests | Piper TTS + Parakeet STT |
| `calendar/` | Medium | 2 test files | Bridge + fake bridge — covered |

---

## Execution Order (Updated)

```
Next up:  Phase 1 — memory/store core tests (SaveFact, SaveMessage, GetContextFacts, PIIVault, etc.)
          Phase 2 — scrub/ tests (security-critical, zero coverage)
Then:     Phase 3 — tool handler tests (start with P1 memory + communication tools)
          Phase 1 — memory/context.go, memory/extract.go
Later:    Phase 4 — LLM client expansion, embed client expansion
          Phase 5 — config loading, layers expansion, scheduler expansion
Low pri:  Remaining tool handlers, persona, tui, vision, voice
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

- **Phase 1 target:** Memory store has 15+ test cases, all core CRUD operations covered
- **Phase 2 target:** scrub/ has full pattern coverage (8+ cases)
- **Phase 3 target:** Every tool handler has at least 1 happy-path test
- **Phase 4 target:** All HTTP clients have httptest-based tests
- **Full buildout:** `go test ./...` runs in under 30 seconds, covers all packages with .go files
- **Ongoing:** Every bug fix ships with a regression test

**Current score:** 20/52 packages tested (38%)
**Target:** 40+/52 packages tested (77%+) — some packages (logger, tui, voice) may stay untested if they're thin wrappers
