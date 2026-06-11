-- Allow multiple scheduler_tasks rows per kind (user-created schedules).
-- Previously kind had a UNIQUE index, limiting to one row per handler.

DROP INDEX IF EXISTS idx_scheduler_tasks_kind;

ALTER TABLE scheduler_tasks ADD COLUMN source TEXT NOT NULL DEFAULT 'yaml';
ALTER TABLE scheduler_tasks ADD COLUMN name TEXT;
ALTER TABLE scheduler_tasks ADD COLUMN enabled INTEGER NOT NULL DEFAULT 1;

-- Non-unique composite index for loader lookups (WHERE kind=? AND source='yaml').
CREATE INDEX idx_scheduler_tasks_kind_source ON scheduler_tasks(kind, source);
