# D1 Shared State Migration Plan

## Context

The bot runs on two machines — Mac Mini (always-on prod) and MacBook (dev sessions). The CF Worker routes messages to one machine at a time, so only one ever writes. Both machines use the same `her.db` — there is no separate dev database. Right now, each machine's `her.db` is isolated; the Mac Mini's data is stale (early project days) while the MacBook has all the real data.

**Goal:** Share core identity data (memories, messages, persona, reflections, moods) via Cloudflare D1 so both machines see the same person. D1 is the source of truth. Local `her.db` stays for fast reads and embeddings (sqlite-vec). Writes go to both local SQLite and D1. On sync triggers, machines pull from D1 to catch up. `her dev` also uses `her.db` (no separate dev database — the sim handles clean-slate testing).

---

## Architecture

```
             ┌─────────────┐
             │  Cloudflare  │
             │     D1       │  ← source of truth (10 synced tables)
             └──────┬───────┘
                    │
         ┌──────────┼──────────┐
         ▼                     ▼
  ┌──────────────┐     ┌──────────────┐
  │  Mac Mini    │     │  MacBook     │
  │  SyncedStore │     │  SyncedStore │
  │  ┌─────────┐ │     │  ┌─────────┐ │
  │  │ her.db  │ │     │  │ her.db  │ │
  │  │ (local) │ │     │  │ (local) │ │
  │  └─────────┘ │     │  └─────────┘ │
  └──────────────┘     └──────────────┘

Writes: local SQLite first → D1 push in background
Reads:  always from local SQLite (fast, has embeddings)
Sync:   pull from D1 → upsert into local SQLite → rebuild embeddings
```

### SyncedStore Decorator Pattern

```go
type SyncedStore struct {
    *SQLiteStore           // all 101 methods pass through
    d1     *d1.Client
    pushCh chan d1.PushJob // buffered channel, background goroutine drains
}

// Override write methods for synced tables only (~25 methods).
// Other ~76 methods pass through via embedding unchanged.
func (s *SyncedStore) SaveMemory(...) (int64, error) {
    id, err := s.SQLiteStore.SaveMemory(...)
    if err != nil { return 0, err }
    s.pushCh <- d1.PushJob{Table: "memories", ID: id, Op: "upsert"}
    return id, nil
}
```

---

## Tables Synced to D1

| Table | ID Column | Timestamp Column | Notes |
|-------|-----------|-----------------|-------|
| `messages` | id | timestamp | Core conversation history |
| `summaries` | id | timestamp | Compaction summaries |
| `memories` | id | timestamp | **No embedding/embedding_text columns** |
| `memory_links` | (source_id, target_id) | created_at | Graph edges |
| `reflections` | id | timestamp | Dream journal entries |
| `persona_versions` | id | timestamp | Persona snapshots |
| `traits` | id | timestamp | Personality scores |
| `persona_state` | id (always 1) | last_reflection_at | Singleton row |
| `mood_entries` | id | ts | **No embedding column** |

Plus a `_sync_meta` tracking table in both D1 and local SQLite.

### Tables That Stay Local-Only

vec_memories, vec_moods, metrics, agent_turns, searches, classifier_log, command_log, pii_vault, pending_confirmations, pending_mood_proposals, scheduled_tasks, calendar_events, expenses, expense_items, inbox, location_history, facts (legacy)

---

## Sync Protocol

**ID-based, not timestamp-based.** Since only one machine writes at a time (CF Worker routing), auto-increment IDs never conflict between machines. Each machine tracks `last_synced_id` per table in `_sync_meta`.

**Push (after local write):**
- Background goroutine reads from `pushCh`
- Batches multiple jobs, sends to D1 via REST API
- On failure: log and retry with backoff. Local write already succeeded — D1 catches up eventually.

**Pull (on sync trigger):**
- For each synced table concurrently: `SELECT * FROM {table} WHERE id > ? ORDER BY id`
- `INSERT OR REPLACE` into local SQLite
- After pull: bump `sqlite_sequence` to `MAX(local_max_id, d1_max_id)` to prevent ID collisions when this machine resumes writing
- Then trigger embedding backfill for new memories (reuse existing `MemoriesWithoutEmbeddings()` + embed goroutine)

**Sync triggers:**
1. **Startup** — `her run` and `her dev` both pull on start
2. **Dev-session-end** — prod instance polls KV every 30s; when `dev_mode_active` disappears, triggers a pull

---

## Phases

### Phase 1 — Store Interface Extraction
**Goal:** Define a `Store` interface, rename current struct to `SQLiteStore`. Zero behavior change.

**Files changed:**
- `memory/store.go` — rename `Store` → `SQLiteStore`, add `type Store interface { ... }` with all ~88 public methods (excluding `DB()` and `MigrateFromLegacyFacts()`)
- `memory/store_*.go` — rename receiver types (`func (s *Store)` → `func (s *SQLiteStore)`)
- `tools/context.go` — change `Store *memory.Store` → `Store memory.Store` (interface). This cascades to all 20+ tool handlers for free.
- `bot/telegram.go` — change `store *memory.Store` → `store memory.Store`
- ~36 files total reference `*memory.Store` — migrate all callers

**Verification:** `go build ./...` passes. `go test ./...` passes. Zero behavior change.

### Phase 2 — D1 Database & Schema
**Goal:** Create the D1 database and write the schema for synced tables.

- `wrangler d1 create her-db` — creates the D1 database
- Write `d1/schema.sql` — DDL for the 10 synced tables (same SQLite syntax, minus embedding columns)
- Add `_sync_meta` table: `CREATE TABLE _sync_meta (table_name TEXT PRIMARY KEY, last_synced_id INTEGER DEFAULT 0)`
- Apply schema: `wrangler d1 execute her-db --file d1/schema.sql`
- Add `D1DatabaseID` to `CloudflareConfig` in `config/config.go`
- Update `config.yaml.example`

### Phase 3 — D1 Go Client
**Goal:** Thin HTTP client for D1's REST API.

New package: `d1/`
- `d1/client.go` — `Client` struct with `Query(sql string, params ...any) ([]Row, error)` and `Batch(stmts []Statement) error`
- Uses `POST https://api.cloudflare.com/client/v4/accounts/{account_id}/d1/database/{database_id}/query`
- Bearer token auth (reuses `cfg.Cloudflare.APIToken`)
- 10s timeout per request
- JSON request/response marshaling

### Phase 4 — SyncedStore Decorator
**Goal:** Wrap SQLiteStore with D1 push-on-write.

New file: `memory/synced_store.go`
- `SyncedStore` struct embedding `*SQLiteStore` + `*d1.Client` + `pushCh chan PushJob`
- Override ~25 write methods for synced tables (SaveMessage, SaveMemory, SaveMoodEntry, SaveReflection, SavePersonaVersion, SaveSummary, SaveTraits, SetLastReflectionAt, SetLastRewriteAt, UpdateMemory, UpdateMemoryTags, DeactivateMemory, SupersedeMemory, LinkMemories, UpdateMoodEntry, DeleteMoodEntry, UpdateMessageScrubbed, UpdateMessageMedia, UpdateMessageVoicePath, UpdateMessageTokenCount, etc.)
- Background push goroutine: reads from `pushCh`, batches, sends to D1
- `NewSyncedStore(sqlite *SQLiteStore, d1Client *d1.Client) *SyncedStore`
- `Close()` drains push queue before closing

### Phase 5 — Sync Engine (Pull)
**Goal:** Pull from D1, upsert into local SQLite, rebuild embeddings.

New file: `memory/sync.go`
- `(s *SyncedStore) Pull(ctx context.Context) error` — concurrent per-table fetch from D1
- Uses `errgroup` for concurrent table pulls with shared context
- After pull: bump `sqlite_sequence` for each table
- After pull: call existing `MemoriesWithoutEmbeddings()` + batch embed
- Track progress in `_sync_meta`

### Phase 6 — Startup Wiring & KV Poller
**Goal:** Wire SyncedStore into bot startup. Add KV polling on prod.

**Files changed:**
- `cmd/run.go` — if `cfg.Cloudflare.D1DatabaseID != ""`, wrap SQLiteStore in SyncedStore. Run `Pull()` before bot starts. Start KV poller goroutine.
- `cmd/dev.go` — run `Pull()` on dev startup (hydrate local her.db with latest from D1). Remove the `cfg.Memory.DBPath = "./her-dev.db"` override — dev uses the same her.db. On shutdown, write `dev_session_ended=<timestamp>` to KV before clearing dev keys.
- Add KV poller goroutine: checks `dev_session_ended` key every 30s. When found, triggers `Pull()`, then deletes the key.

### Phase 7 — Sync Commands
**Goal:** Manual push/pull commands for initial upload and on-demand sync.

New command group: `her sync push` / `her sync pull`
- `cmd/sync.go` — `her sync push` reads each synced table from local SQLite, batches rows, pushes to D1. `her sync pull` pulls from D1 into local SQLite (same as the automatic Pull but manually triggered).
- Concurrent per-table uploads (same `errgroup` pattern)
- Idempotent: uses `INSERT OR REPLACE`
- Progress logging: "pushing memories: 150/150", "pushing messages: 2340/2340"
- Updates `_sync_meta` with max IDs after push

### Phase 8 — Testing & Polish
- Integration test: push → pull → verify data matches
- Test SyncedStore push failure doesn't block local writes
- Test Pull correctly skips already-synced rows
- Test embedding backfill runs after pull
- Test KV poller triggers sync
- Verify `go test ./... -race` passes

---

## Config Changes

```yaml
cloudflare:
  account_id: "..."
  api_token: "${CLOUDFLARE_API_TOKEN}"
  kv_namespace_id: "..."
  d1_database_id: ""   # NEW — empty = D1 sync disabled, SyncedStore not used
```

When `d1_database_id` is empty, the bot runs with plain SQLiteStore — no D1 overhead. This makes the feature opt-in and backwards compatible.

---

## Method Override List (Phase 4)

Write methods that touch synced tables — these get overridden in SyncedStore:

**messages:** SaveMessage, UpdateMessageScrubbed, UpdateMessageMedia, UpdateMessageVoicePath, UpdateMessageTokenCount
**summaries:** SaveSummary
**memories:** SaveMemory, UpdateMemory, UpdateMemoryTags, DeactivateMemory, SupersedeMemory, UpdateMemoryEmbedding (sync text fields only, skip embedding)
**memory_links:** LinkMemories (via AutoLinkMemory path)
**reflections:** SaveReflection
**persona:** SavePersonaVersion, SaveTraits, SetLastReflectionAt, SetLastRewriteAt
**mood:** SaveMoodEntry, UpdateMoodEntry, DeleteMoodEntry

Read methods, local-only table methods, and vector operations pass through unchanged.

---

## Verification

After each phase:
1. `go build ./...` — compiles
2. `go test ./...` — all tests pass
3. `go test -race ./...` — no data races

End-to-end test (after all phases):
1. `her sync push` — upload current MacBook data to D1
2. `her sync pull` on Mac Mini — verify it gets everything
3. `her dev` — verify pull on startup hydrates local her.db
4. Send messages during dev → verify they push to D1
5. Stop dev → verify prod syncs new data from D1 via KV poller
6. `her run` on Mac Mini — verify it has everything
