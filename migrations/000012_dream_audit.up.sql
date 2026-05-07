-- Dream audit log — tracks every operation the memory dreamer performs.
-- Soft-delete only: expired/merged memories are deactivated, never hard-deleted.
-- Dry-run mode logs operations with dry_run=1 but skips DB mutations.
CREATE TABLE IF NOT EXISTS dream_audit (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
    operation TEXT NOT NULL,          -- 'merge', 'expire', 'promote', 'split'
    source_ids TEXT NOT NULL,         -- JSON array of affected memory IDs
    result_id INTEGER,                -- new memory ID (merge) or updated ID (promote)
    before_text TEXT,                 -- original text (promote) or summary (merge sources)
    after_text TEXT,                  -- new/updated text
    reason TEXT,                      -- LLM's reasoning for the operation
    dry_run BOOLEAN DEFAULT 0         -- 1 = logged only, no DB changes made
);
