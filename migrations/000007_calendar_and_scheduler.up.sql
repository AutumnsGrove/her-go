-- Calendar events and scheduler tasks

CREATE TABLE IF NOT EXISTS calendar_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    event_id TEXT,
    title TEXT NOT NULL,
    start TEXT NOT NULL,
    end TEXT NOT NULL,
    location TEXT,
    notes TEXT,
    calendar TEXT NOT NULL,
    active BOOLEAN DEFAULT 1,
    job TEXT,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME
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

CREATE INDEX IF NOT EXISTS idx_calendar_events_start_end ON calendar_events(start, end);
CREATE INDEX IF NOT EXISTS idx_calendar_events_event_id ON calendar_events(event_id);
CREATE INDEX IF NOT EXISTS idx_calendar_events_job ON calendar_events(job);
CREATE INDEX IF NOT EXISTS idx_scheduler_tasks_next_fire ON scheduler_tasks(next_fire);
CREATE UNIQUE INDEX IF NOT EXISTS idx_scheduler_tasks_kind ON scheduler_tasks(kind);
