-- Add write-time retrieval hooks to memories. A hook is a single short line
-- written by the agent when it saves the memory, phrased as a trigger
-- condition ("call her this when a stranger uses a pet name on her") rather
-- than a restatement of the fact. It lets the driver decide relevance by
-- reading the card/self-memory index, instead of running a semantic search
-- on every turn. See docs/plans/PLAN-memory-hooks-dual-recall.md.
--
-- slug is a secondary, human-readable identifier (e.g. "donette-bit") that
-- resolves to the same row as id. It is NOT the primary key — id stays
-- canonical, slug is just a friendlier way to reference a memory in
-- recall_memories(slug=...) than remembering a bare integer.
--
-- Both columns are nullable: existing memories degrade to search-only until
-- backfilled (see cmd/backfill_hooks.go) or naturally superseded.

ALTER TABLE memories ADD COLUMN hook TEXT;
ALTER TABLE memories ADD COLUMN slug TEXT;

CREATE INDEX IF NOT EXISTS idx_memories_slug ON memories(slug)
    WHERE slug IS NOT NULL;
