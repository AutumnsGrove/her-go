-- Observability overhaul: supersession tracking, classifier verdicts, inter-agent comms
--
-- This migration adds full parity with production her.db schema so sim reports
-- can show:
-- - Which memories superseded which (supersession chains)
-- - Why they were updated (supersede_reason)
-- - Classifier verdicts (LOW_VALUE, SPLIT, ESCALATION, etc.)
-- - Soft verdict rewrites
-- - Inter-agent communication (send_task, notify_agent)

-- ── Supersession tracking for sim_memories ──────────────────────────────────
ALTER TABLE sim_memories ADD COLUMN superseded_by INTEGER REFERENCES sim_memories(id);
ALTER TABLE sim_memories ADD COLUMN supersede_reason TEXT;
ALTER TABLE sim_memories ADD COLUMN tags TEXT;
ALTER TABLE sim_memories ADD COLUMN context TEXT;
ALTER TABLE sim_memories ADD COLUMN source_message_id INTEGER;

-- ── Classifier verdicts (all gates: memory, self_memory, reply safety, style) ──
CREATE TABLE IF NOT EXISTS sim_classifier_log (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id INTEGER NOT NULL REFERENCES sim_runs(id),
    timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
    conversation_id TEXT,
    write_type TEXT NOT NULL,    -- "memory", "self_memory_safety", "self_memory", "reply_safety", "reply"
    verdict TEXT NOT NULL,        -- "SAVE", "LOW_VALUE", "SPLIT", "ESCALATION", "PURE_VALIDATION", etc.
    content TEXT NOT NULL,        -- the proposed memory/reply text
    reason TEXT,                  -- classifier's explanation
    rewrite TEXT,                 -- soft verdict rewrite suggestion (for ESCALATION, STYLE_ISSUE, etc.)
    accepted BOOLEAN              -- did agent use the rewrite? (NULL = not applicable)
);

-- Index for report queries (classifier verdicts by verdict type)
CREATE INDEX IF NOT EXISTS idx_sim_classifier_verdict ON sim_classifier_log(run_id, verdict);

-- ── Inter-agent communication (send_task, notify_agent inbox) ──────────────
CREATE TABLE IF NOT EXISTS sim_inbox_messages (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    run_id INTEGER NOT NULL REFERENCES sim_runs(id),
    timestamp DATETIME DEFAULT CURRENT_TIMESTAMP,
    task_type TEXT NOT NULL,      -- "cleanup", "split", "update", etc.
    note TEXT,                     -- human-readable task description
    memory_ids TEXT,               -- JSON array of affected memory IDs (e.g., "[14, 16]")
    processed BOOLEAN DEFAULT 0,  -- did the memory agent handle this task?
    result TEXT                    -- outcome summary (optional)
);

-- ── Updated view: include active/superseded counts ─────────────────────────
DROP VIEW IF EXISTS run_summary;
CREATE VIEW run_summary AS
SELECT
    r.id,
    r.timestamp,
    r.suite_name,
    r.agent_model,
    r.chat_model,
    r.memory_model,
    r.total_cost_usd,
    r.duration_ms,
    COUNT(DISTINCT CASE WHEN f.active = 1 THEN f.id END) AS active_memories,
    COUNT(DISTINCT CASE WHEN f.active = 0 THEN f.id END) AS superseded_memories,
    COUNT(DISTINCT f.id) AS total_memories,
    COUNT(DISTINCT m.id) / 2 AS turns,
    COUNT(DISTINCT c.id) FILTER (WHERE c.verdict != 'SAVE' AND c.verdict != 'PASS' AND c.verdict != 'SAFE') AS classifier_rejections
FROM sim_runs r
LEFT JOIN sim_memories f ON f.run_id = r.id
LEFT JOIN sim_messages m ON m.run_id = r.id AND m.role = 'user'
LEFT JOIN sim_classifier_log c ON c.run_id = r.id
GROUP BY r.id;
