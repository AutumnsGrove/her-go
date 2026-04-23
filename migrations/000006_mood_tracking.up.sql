-- Mood tracking tables

CREATE TABLE IF NOT EXISTS mood_entries (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    ts              DATETIME DEFAULT CURRENT_TIMESTAMP,
    kind            TEXT NOT NULL,
    valence         INTEGER NOT NULL,
    labels          TEXT NOT NULL DEFAULT '[]',
    associations    TEXT NOT NULL DEFAULT '[]',
    note            TEXT NOT NULL DEFAULT '',
    source          TEXT NOT NULL,
    confidence      REAL NOT NULL DEFAULT 0,
    conversation_id TEXT,
    embedding       BLOB,
    created_at      DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME
);

CREATE TABLE IF NOT EXISTS pending_mood_proposals (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    ts                  DATETIME DEFAULT CURRENT_TIMESTAMP,
    telegram_chat_id    INTEGER NOT NULL,
    telegram_message_id INTEGER NOT NULL,
    proposal_json       TEXT NOT NULL,
    status              TEXT NOT NULL DEFAULT 'pending',
    expires_at          DATETIME NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_mood_entries_ts ON mood_entries(ts);
CREATE INDEX IF NOT EXISTS idx_mood_entries_kind_ts ON mood_entries(kind, ts);
CREATE INDEX IF NOT EXISTS idx_pending_mood_status_expires ON pending_mood_proposals(status, expires_at);
