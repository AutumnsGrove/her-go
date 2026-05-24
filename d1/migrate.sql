-- D1 Migration: add columns and tables that were added after initial schema.
-- Run via: npx wrangler d1 execute her-db --file d1/migrate.sql --remote
--
-- Safe to re-run: uses ALTER TABLE ADD COLUMN (errors are non-fatal in batch),
-- and CREATE TABLE/INDEX IF NOT EXISTS.

-- Migration 000014: mood supersede columns
ALTER TABLE mood_entries ADD COLUMN superseded_by INTEGER REFERENCES mood_entries(id);
ALTER TABLE mood_entries ADD COLUMN supersede_reason TEXT;

-- Index for supersede lookups (depends on column above)
CREATE INDEX IF NOT EXISTS idx_mood_entries_superseded ON mood_entries(superseded_by);

-- New tables from VPS branch (d1/schema.sql already has these but they may
-- not exist yet if the schema was applied before these were added)

CREATE TABLE IF NOT EXISTS pii_vault (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    message_id     INTEGER,
    token          TEXT NOT NULL,
    original_value TEXT NOT NULL,
    entity_type    TEXT NOT NULL,
    created_at     DATETIME DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (message_id) REFERENCES messages(id)
);

CREATE TABLE IF NOT EXISTS metrics (
    id                INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp         DATETIME DEFAULT CURRENT_TIMESTAMP,
    model             TEXT NOT NULL,
    prompt_tokens     INTEGER,
    completion_tokens INTEGER,
    total_tokens      INTEGER,
    cost_usd          REAL,
    latency_ms        INTEGER,
    message_id        INTEGER,
    is_fallback       BOOLEAN DEFAULT 0,
    agent_role        TEXT DEFAULT '',
    FOREIGN KEY (message_id) REFERENCES messages(id)
);

CREATE TABLE IF NOT EXISTS agent_turns (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp   DATETIME DEFAULT CURRENT_TIMESTAMP,
    message_id  INTEGER,
    turn_index  INTEGER,
    role        TEXT NOT NULL,
    tool_name   TEXT,
    tool_args   TEXT,
    content     TEXT,
    FOREIGN KEY (message_id) REFERENCES messages(id)
);

CREATE TABLE IF NOT EXISTS dream_audit (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp   DATETIME DEFAULT CURRENT_TIMESTAMP,
    operation   TEXT NOT NULL,
    source_ids  TEXT NOT NULL,
    result_id   INTEGER,
    before_text TEXT,
    after_text  TEXT,
    reason      TEXT,
    dry_run     BOOLEAN DEFAULT 0
);

CREATE TABLE IF NOT EXISTS scheduler_tasks (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    kind               TEXT NOT NULL,
    cron_expr          TEXT,
    next_fire          DATETIME NOT NULL,
    payload_json       TEXT NOT NULL DEFAULT '{}',
    retry_max_attempts INTEGER NOT NULL DEFAULT 0,
    retry_backoff      TEXT NOT NULL DEFAULT 'none',
    retry_initial_wait INTEGER NOT NULL DEFAULT 0,
    last_run_at        DATETIME,
    last_error         TEXT,
    attempt_count      INTEGER NOT NULL DEFAULT 0,
    created_at         DATETIME DEFAULT CURRENT_TIMESTAMP
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_scheduler_tasks_kind ON scheduler_tasks(kind);
