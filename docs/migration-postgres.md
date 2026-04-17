# PostgreSQL Migration Plan

**Status:** Planning  
**Scope:** Replace SQLite (her.db) with PostgreSQL + pgvector for the production store.  
**D1 goal:** Define a `RemoteStore` interface for eventual Cloudflare D1 sync (facts, messages, persona, reflections — no vectors).

---

## Motivation

The current SQLite + sqlite-vec setup works, but has two problems:

1. **Vector search is bolted on.** sqlite-vec is a CGo extension that requires the right shared library at runtime. pgvector is a first-class Postgres extension with proper HNSW indexing, better query planning, and a native float32 vector type — no BLOB serialization, no manual upsert-via-delete pattern.

2. **D1 compatibility is hard to bolt on later.** The eventual goal is to sync core identity data (facts, messages, persona versions, reflections) to Cloudflare D1 so her can run on multiple machines with shared state. Defining a clean `RemoteStore` interface now means D1 gets a proper contract to implement against, not a retrofit.

---

## Stack Decisions

| Concern | Choice | Why |
|---|---|---|
| Database | PostgreSQL 16 + pgvector | First-class vector support, HNSW indexing, proper types |
| Driver | pgx/v5 | Best PG driver in Go. Native pgvector support. Better perf than `database/sql` wrapper |
| Query layer | sqlc | Write SQL, get type-safe Go. Like Drizzle — schema is truth, no ORM magic |
| Migrations | golang-migrate | Plain versioned `.sql` up/down files. CLI + embedded. Easy to audit for D1 compat |
| Local dev | Docker Compose | Reproducible, version-pinned, no system install required |
| Config | config.yaml only | Single source of truth. `database.host/port/user/pass/name` section |
| Vector index | HNSW | Best recall/latency tradeoff at this scale. IVFFlat needs a training step |
| Sim stores | SQLite (unchanged) | Temp clean-room stays SQLite — zero ceremony, fast, throwaway |

---

## Architecture

```
Production (after migration)
──────────────────────────────────────────────────────
cmd/run.go
  └── memory.NewPGStore(cfg)         ← new PGStore implementation
        ├── pgx/v5 connection pool
        ├── pgvector for embeddings
        └── implements Store interface

Simulation (unchanged)
──────────────────────────────────────────────────────
cmd/sim.go
  └── memory.NewSQLiteStore(tmpPath) ← renamed from current NewStore()
        ├── database/sql + go-sqlite3
        ├── sqlite-vec for KNN
        └── implements Store interface

Remote sync (future — interface only now)
──────────────────────────────────────────────────────
RemoteStore interface
  ├── ReadFacts() / WriteFact()
  ├── ReadMessages() / WriteMessage()
  ├── ReadPersonaVersions() / WritePersonaVersion()
  └── ReadReflections() / WriteReflection()
  (D1 client implements this when the time comes)
```

---

## Directory Layout (after migration)

```
her-go/
├── db/
│   ├── migrations/
│   │   ├── 000001_initial_schema.up.sql
│   │   ├── 000001_initial_schema.down.sql
│   │   ├── 000002_add_hnsw_index.up.sql
│   │   └── 000002_add_hnsw_index.down.sql
│   ├── queries/
│   │   ├── facts.sql
│   │   ├── messages.sql
│   │   ├── metrics.sql
│   │   ├── tasks.sql
│   │   ├── expenses.sql
│   │   ├── mood.sql
│   │   ├── persona.sql
│   │   ├── misc.sql
│   │   ├── agent.sql
│   │   └── summaries.sql
│   └── sqlc/                        ← generated, do not edit
│       ├── db.go
│       ├── models.go
│       └── *.go (one per query file)
├── memory/
│   ├── store.go                     ← Store interface definition
│   ├── pg_store.go                  ← PGStore: production implementation
│   ├── sqlite_store.go              ← SQLiteStore: sim implementation (current Store renamed)
│   └── remote_store.go              ← RemoteStore interface (D1 contract)
├── scripts/
│   └── migrate_sqlite_to_pg.go      ← one-time data migration, run manually
├── docker-compose.yml
├── sqlc.yaml
└── config.yaml.example              ← updated with database section
```

---

## Config Changes

```yaml
# config.yaml (add this section)
database:
  host: localhost
  port: 5432
  user: her
  password: her
  name: her
  sslmode: disable
```

The production store connection string is assembled from these fields at startup.  
No `DATABASE_URL` env var — everything lives in config.yaml.

---

## Store Interface

The `Store` interface is extracted from the current `memory.Store` struct's public method set. Both `SQLiteStore` (sim) and `PGStore` (production) implement it.

```go
// memory/store.go
type Store interface {
    // Messages
    SaveMessage(role, content, scrubbed, conversationID string) (int64, error)
    RecentMessages(conversationID string, limit int) ([]Message, error)
    // ... all current public methods

    // Lifecycle
    Close() error
}
```

`cmd/run.go` gets a `Store` variable (interface type). `cmd/sim.go` calls `memory.NewSQLiteStore()`. `cmd/run.go` calls `memory.NewPGStore()`. Everything else in `agent/`, `tools/`, `persona/` continues to use the interface — no changes needed there.

---

## Vector Storage Change

**Before (sqlite-vec):**
```go
// Facts table: embedding BLOB (float32 little-endian bytes)
// Virtual table: vec_facts USING vec0(embedding float[768])
// KNN query:
SELECT rowid, distance FROM vec_facts
WHERE embedding MATCH ? AND k = ?
```

**After (pgvector):**
```go
// Facts table: embedding vector(768), embedding_text vector(768)
// Index: CREATE INDEX facts_embedding_hnsw ON facts
//        USING hnsw (embedding vector_cosine_ops)
// KNN query:
SELECT id, embedding <=> $1 AS distance
FROM facts
WHERE active = true
ORDER BY embedding <=> $1
LIMIT $2
```

No virtual table, no BLOB serialization, no upsert-via-delete. pgvector's `<=>` operator is cosine distance — same semantics as sqlite-vec's `distance_metric=cosine`.

The `embed/embed.go` client is unchanged — it still returns `[]float32`. The PGStore converts this to pgvector's `pgvector.Vector` type before writing.

---

## D1 Compatibility Scope

D1 is Cloudflare's SQLite-compatible edge database. It does not support pgvector. The sync scope is intentionally limited to tables that do not need vectors:

| Table | D1? | Notes |
|---|---|---|
| `facts` | ✓ | Text only — no `embedding` or `embedding_text` columns |
| `messages` | ✓ | Full content, both raw and scrubbed |
| `persona_versions` | ✓ | Full persona snapshots |
| `reflections` | ✓ | Journal entries |
| `metrics` | ✗ | Machine-specific, not useful cross-machine |
| `mood_entries` | ✗ | Future consideration |
| `expenses` | ✗ | Future consideration |
| `scheduled_tasks` | ✗ | Machine-specific scheduling |
| `pii_vault` | ✗ | Sensitive — do not sync |

The `RemoteStore` interface is defined in Phase 6 but not implemented. When D1 sync is built, the D1 client implements `RemoteStore` and a background goroutine handles the push after each write.

---

## Migration Phases

### Phase 1 — Infrastructure
- Add `docker-compose.yml` with `postgres:16` + `pgvector/pgvector:pg16` image
- Add `database` section to `config.yaml.example` and `config/config.go`
- Add `pgx/v5`, `pgvector/pgvector-go`, `golang-migrate` to `go.mod`
- Create `db/migrations/` directory

### Phase 2 — Schema (golang-migrate)
- Write `000001_initial_schema.up.sql` — full DDL for all 16 tables, adapted for Postgres syntax
  - `SERIAL` / `BIGSERIAL` instead of `INTEGER PRIMARY KEY AUTOINCREMENT`
  - `BOOLEAN` instead of `INTEGER` (0/1)
  - `JSONB` for `payload` and `tags` columns
  - `vector(768)` for embedding columns (dimension from config)
  - All foreign keys, indexes
- Write `000002_add_hnsw_index.up.sql` — HNSW index on `facts.embedding`
- Write corresponding `.down.sql` files

### Phase 3 — sqlc Setup
- Write `sqlc.yaml` pointing at `db/migrations/` (schema) and `db/queries/` (queries)
- Write all query files in `db/queries/` — one SQL file per store file in `memory/`
  - Annotated with `-- name: FunctionName :one/:many/:exec`
  - Vector queries use `$1::vector` parameter casting
- Run `sqlc generate` → populates `db/sqlc/`

### Phase 4 — Store Interface Extraction
- Define `Store` interface in `memory/store.go` from the current struct's public methods
- Rename current `memory/store.go` + `memory/store_*.go` to `memory/sqlite_store.go` (and siblings)
- Rename `NewStore()` → `NewSQLiteStore()`
- Update `cmd/sim.go` to call `memory.NewSQLiteStore()`
- Verify: everything still compiles and sim still runs

### Phase 5 — PGStore Implementation
- Create `memory/pg_store.go` with `PGStore` struct (wraps `pgx.Pool`)
- Implement all `Store` interface methods using generated sqlc code
- Key differences from SQLiteStore:
  - `pgvector.Vector` instead of `sqlite_vec.SerializeFloat32`
  - `<=>` cosine distance operator instead of `MATCH`
  - `pgx.Pool` instead of `*sql.DB`
  - `ctx context.Context` threading (pgx requires context everywhere)
- Port all 16 store files' logic

### Phase 6 — RemoteStore Interface
- Define `RemoteStore` interface in `memory/remote_store.go`
- Document the D1 sync contract (read/write for the 4 core tables)
- No implementation — interface only

### Phase 7 — Startup Integration
- Update `cmd/run.go` to:
  - Load `database` config
  - Open pgx pool
  - Run golang-migrate on startup (apply pending migrations)
  - Call `memory.NewPGStore(pool)`
- Update all other `cmd/*.go` files that use the store (`retag`, `relink`, `shape`)

### Phase 8 — Data Migration Script
- Write `scripts/migrate_sqlite_to_pg.go`
- Reads `her.db` SQLite tables in dependency order (messages → facts → fact_links → ...)
- Converts:
  - `INTEGER` primary keys → inserted with explicit IDs, sequences reset after
  - `BLOB` embeddings → `pgvector.Vector`
  - `DATETIME` strings → `time.Time`
  - `BOOLEAN` integers → Go `bool`
- Idempotent: skip rows where ID already exists in Postgres
- Run once manually: `go run scripts/migrate_sqlite_to_pg.go`

### Phase 9 — Cleanup
- Remove `sqlite-vec` dependency from production build
- Remove `go-sqlite3` from production imports (keep only in sim files)
- Update `CLAUDE.md` and `SPEC.md` to reflect new stack
- Archive `MIGRATION.md` or update status to "Complete"

---

## Data Migration Details

Migration script reads her.db in this order (respects FK dependencies):

```
1. messages
2. pii_vault           (→ messages)
3. summaries
4. facts
5. fact_links          (→ facts × facts)
6. metrics             (→ messages)
7. scheduled_tasks     (→ messages)
8. expenses
9. expense_items       (→ expenses)
10. mood_entries
11. persona_versions
12. traits             (→ persona_versions)
13. reflections
14. agent_turns        (→ messages)
15. searches           (→ messages)
16. command_log
17. classifier_log
18. location_history
19. pending_confirmations
20. persona_state
```

After inserting all rows with explicit IDs, reset all sequences:
```sql
SELECT setval(pg_get_serial_sequence('facts', 'id'), MAX(id)) FROM facts;
-- (repeat for all tables with BIGSERIAL)
```

---

## Testing the Migration

1. `docker compose up -d` → Postgres running locally
2. Update `config.yaml` with database section
3. `go run scripts/migrate_sqlite_to_pg.go` → data migrated
4. `go run main.go run` → production bot running on Postgres
5. `her sim --suite sims/getting-to-know-you.yaml` → sim still running on SQLite
6. Verify: messages save, facts save, semantic search returns results

---

## Risks and Mitigations

| Risk | Mitigation |
|---|---|
| HNSW index missing at query time | Build index in a separate migration after initial data load |
| Sequence desync after manual ID inserts | Reset all sequences at end of migration script |
| pgvector dimension mismatch | Migration validates `cfg.Embed.Dimension` matches schema before writing |
| D1 SQL dialect diverges from Postgres | Keep D1-bound tables using only portable SQL in their migrations |
| sim depends on sqlite-vec being installed | sim uses SQLiteStore — no change, sqlite-vec stays in sim path |
