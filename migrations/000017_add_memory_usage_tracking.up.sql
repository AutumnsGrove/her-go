-- Track how often and how recently memories are recalled into the chat
-- prompt. This powers the blended retrieval scoring (Feature 1: Importance
-- Rewire) — memories that are actually used in conversation get a usage
-- boost, replacing the one-shot LLM importance judgment as the long-term
-- signal.

ALTER TABLE memories ADD COLUMN last_recalled_at TIMESTAMP;
ALTER TABLE memories ADD COLUMN recall_count INTEGER NOT NULL DEFAULT 0;

-- Index for the dreamer's recalibration pass and for the forgetting
-- policy (Feature 3) which needs to find memories unused for 60+ days.
CREATE INDEX IF NOT EXISTS idx_memories_recall
    ON memories (active, recall_count, last_recalled_at);
