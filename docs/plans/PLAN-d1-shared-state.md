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

vec_memories, vec_moods, metrics, agent_turns, searches, classifier_log, command_log, pii_vault, pending_confirmations, pending_mood_proposals, scheduled_tasks, calendar_events, inbox, location_history, facts (legacy)

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

### Phase 1 — Store Interface Extraction ✅
**Status:** Complete — commit `9ac74ca`

Defined `Store` interface (87 methods including `GetEmbedDimension()`), renamed `Store` struct to `SQLiteStore`. 58 files updated — all callers now use `memory.Store` (interface). Added `GetEmbedDimension()` getter since interfaces can't expose fields. `go build ./...` and `go test ./...` pass.

### Phase 2 — D1 Database & Schema ✅
**Status:** Complete — commits `0897dc0`, `44b1cd3`

- D1 database `her-db` created (ID in config.yaml `cloudflare.d1_database_id`)
- `d1/schema.sql` written with 9 synced tables + `_sync_meta` (embedding columns excluded)
- Schema applied via `wrangler d1 execute --remote`
- `D1DatabaseID` added to `CloudflareConfig` in `config/config.go`
- D1 binding added to `worker/wrangler.toml` for convenience
- `config.yaml.example` updated

### Phase 3 — D1 Go Client ✅
**Status:** Complete — commit `90e7220`

- `d1/client.go` — `Client` struct with `Query()` and `Batch()` methods
- Uses Cloudflare REST API `/query` endpoint (rows as objects)
- 30s timeout, bearer auth via `cfg.Cloudflare.APIToken`
- 6 tests using `httptest.Server` (no real Cloudflare calls)

### Phase 4 — SyncedStore Decorator ✅
**Status:** Complete — commit `73edb9b`

**Design change from original plan:** Replaced in-memory channel with **transactional outbox pattern** for crash safety. Writes land in `_d1_outbox` SQLite table (durable across crashes), carrier goroutine reads actual row data at send-time and pushes to D1 in batches.

New file: `memory/synced_store.go` (725 lines)
- `SyncedStore` struct embedding `*SQLiteStore` + `*d1.Client` + notify/done channels
- 20 method overrides — each calls SQLiteStore then `writeOutbox(table, id, op)`
- `_d1_outbox` table created on init (id, table_name, row_id, row_id_2, op, created_at)
- Carrier goroutine: polls outbox, reads rows via `syncedTableSpecs` registry, batches to D1, deletes on success
- `syncedTableSpecs` — column layouts for all 9 synced tables (single source of truth)
- Composite key support via `row_id_2` column for `memory_links`
- `NewSyncedStore(sqlite, d1Client) (*SyncedStore, error)` — returns error because `initOutbox()` can fail
- `Close()` signals carrier to drain, waits, then closes SQLiteStore
- Startup reconciliation (comparing local MAX(id) vs D1) covers the tiny crash window between data write and outbox insert

### Phase 5 — Sync Engine (Pull) ✅
**Status:** Complete — commit `6e9869e`

New file: `memory/sync.go`
- `Pull(ctx)` — concurrent per-table fetch from D1 via errgroup
- Incremental pull (WHERE id > last_synced_id) for 7 tables with pagination (500 rows/page)
- Full pull for composite-key/singleton tables (memory_links, persona_state)
- `_sync_meta` tracking table for pull progress
- `sqlite_sequence` bumping to prevent ID collisions
- FK checks disabled during pull to handle concurrent table inserts
- Embedding backfill delegated to caller (run.go startup handles it)

### Phase 6 — Startup Wiring & KV Poller ✅
**Status:** Complete — commit `6e9869e`

- `cmd/run.go` — wraps SQLiteStore in SyncedStore when `d1_database_id` is set. Runs `Pull()` before bot starts. Passes `botStore` (interface) to all downstream consumers.
- `cmd/run.go` — KV sync poller goroutine (prod only): checks `dev_session_ended` every 30s, triggers Pull, deletes key. Cancelled during shutdown alongside dreamer/scheduler.
- `cmd/dev.go` — removed `her-dev.db` override; dev uses same `her.db` synced via D1. Writes `dev_session_ended` timestamp to KV on shutdown.
- `cmd/dev.go` — added `kvClient.get()` method for reading KV values.
- `d1/client.go` — added `WithBaseURL()` for test httptest.Server injection.

### Phase 7 — Sync Commands ✅
**Status:** Complete — commit `ade3d80`

New file: `cmd/sync.go`
- `her sync push` — bulk upload all local SQLite to D1. Streams rows per-table, batches of 50, concurrent via errgroup. Progress logging.
- `her sync pull` — manual pull from D1 (same as startup Pull but on-demand).
- `PushAll(ctx)` method on SyncedStore for the push logic.
- `makeSyncedStore` helper handles resource cleanup on error paths.
- Updates `_sync_meta` after push so future pulls skip known rows.

### Phase 8 — Testing & Polish ✅
**Status:** Complete

New file: `memory/sync_test.go` — 4 tests with fake D1 server (in-memory SQLite behind httptest.Server):
- `TestPushPullRoundtrip` — push messages/memories/reflections, pull into fresh store, verify data matches
- `TestPushFailureDoesNotBlockLocalWrites` — D1 returns 500, local write still succeeds, outbox entry created
- `TestPullSkipsAlreadySyncedRows` — pull twice, second pull only fetches new rows via _sync_meta cursor
- `TestPullBumpsSequence` — after pulling ID 100 from D1, next local write gets ID > 100
- `go test ./... -race` passes across all 18 packages with zero data races

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
