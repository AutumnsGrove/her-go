-- Tomorrow's preload: a short note the dream cycle writes about what
-- to be ready to bring up in tomorrow's conversation. Auto-injected
-- into the first chat turn of the day, then consumed so it doesn't
-- linger. Old rows kept for audit (mirrors memory_log pattern).

CREATE TABLE IF NOT EXISTS tomorrow_preload (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    generated_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at TIMESTAMP NOT NULL,
    content TEXT NOT NULL,
    consumed BOOLEAN NOT NULL DEFAULT 0,
    consumed_at TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_tomorrow_preload_active
    ON tomorrow_preload (consumed, expires_at);
