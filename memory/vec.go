// Vector-index lifecycle: dimension checking, rebuilding, and re-embedding.
//
// Why this file exists (docs/plans/PLAN-nova-migration.md, step 2): the
// embedding model moved from a local 768-dimension model (nomic-embed-text via
// Ollama) to a remote 1024-dimension one (perplexity/pplx-embed-v1-0.6b via
// OpenRouter). An existing her.db still has `vec_memories` and `vec_moods`
// created with `float[768]`.
//
// The subtle failure mode: sqlite-vec's virtual tables are created with
//
//	CREATE VIRTUAL TABLE IF NOT EXISTS vec_memories USING vec0(embedding float[N] ...)
//
// `IF NOT EXISTS` is a no-op when the table is already there, so on an old
// database the 768-dim table silently survives — and then every insert of a
// 1024-dim vector fails, or (worse) every cosine comparison returns "not
// similar" because the vectors are incompatible. Silent, not loud. So:
//
//  1. Opening a store now compares the existing table's dimension against the
//     configured one and refuses to proceed with an actionable error.
//  2. `her reembed` (cmd/reembed.go) drops and rebuilds both tables at the new
//     dimension and re-embeds every stored row from its text.
package memory

import (
	"database/sql"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// vecDimensionRE extracts N from a sqlite-vec table definition such as
//
//	CREATE VIRTUAL TABLE vec_memories USING vec0(embedding float[768] distance_metric=cosine)
//
// sqlite-vec exposes no catalog column for this, so the DDL text is the only
// source of truth. A regexp is greedy-free here on purpose: `[^]]*` keeps the
// match inside the brackets so a future table with a second column can't
// confuse it.
var vecDimensionRE = regexp.MustCompile(`embedding\s+float\s*\[\s*(\d+)\s*\]`)

// vecTables are the sqlite-vec virtual tables her-go maintains, in the order a
// rebuild should recreate them. Keeping the list in one place means the guard,
// the rebuild, and any future table stay in sync — Data Primacy applied to
// schema names instead of config values.
var vecTables = []string{"vec_memories", "vec_moods"}

// VecDimensionMismatchError reports that the on-disk vector index was built for
// a different embedding model than the one now configured.
//
// It is a concrete type rather than a sentinel so callers (and humans reading a
// log line) get the two numbers that actually matter, and so `errors.As` can
// pull them out without string matching.
type VecDimensionMismatchError struct {
	Table      string // e.g. "vec_memories"
	Existing   int    // dimension the table was created with, e.g. 768
	Configured int    // dimension now requested via embed.dimension, e.g. 1024
}

// Error implements the error interface. In Go, any type with an `Error() string`
// method *is* an error — there is no `implements` keyword and no exception
// class to inherit from. That is why this type can be returned as an `error`
// anywhere and still be type-asserted back later.
func (e *VecDimensionMismatchError) Error() string {
	return fmt.Sprintf(
		"vector index %s was built with %d dimensions but embed.dimension is %d — "+
			"run `her reembed` once to rebuild the index, or set embed.dimension back to %d",
		e.Table, e.Existing, e.Configured, e.Existing,
	)
}

// openOption customizes NewStore. Variadic options keep every existing
// `memory.NewStore(path, dim)` call site compiling while still allowing
// `her reembed` to open a database whose index it intends to rebuild.
//
// This is the functional-options pattern — the Go answer to Python's keyword
// arguments with defaults, which the language does not have for functions with
// many optional parameters.
type openOption func(*openConfig)

type openConfig struct {
	allowVecDimensionMismatch bool
}

// AllowVecDimensionMismatch skips the dimension guard at open time. It exists
// for exactly one caller: `her reembed`, which has to open the store in order to
// fix the mismatch it would otherwise be refused over. Do not use it anywhere
// else — a mismatch that reaches runtime logic is a corrupted index.
func AllowVecDimensionMismatch() openOption {
	return func(c *openConfig) { c.allowVecDimensionMismatch = true }
}

// vecTableDimension returns the dimension an existing sqlite-vec table was
// created with, or 0 when the table does not exist or its DDL cannot be parsed.
//
// Zero means "no opinion" and is deliberately not an error: a missing table is
// normal (fresh database), and an unparseable one is a sqlite-vec internal we
// should not fail the whole app over.
func vecTableDimension(db *sql.DB, table string) (int, error) {
	var ddl sql.NullString
	err := db.QueryRow(
		`SELECT sql FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
	).Scan(&ddl)
	if err == sql.ErrNoRows {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("reading schema for %s: %w", table, err)
	}
	if !ddl.Valid {
		return 0, nil
	}
	m := vecDimensionRE.FindStringSubmatch(ddl.String)
	if len(m) != 2 {
		return 0, nil
	}
	dim, err := strconv.Atoi(strings.TrimSpace(m[1]))
	if err != nil {
		return 0, nil
	}
	return dim, nil
}

// checkVecDimensions compares every existing vector table against the
// configured dimension and returns a *VecDimensionMismatchError for the first
// mismatch. Called from NewStore right after the tables are ensured.
func (s *SQLiteStore) checkVecDimensions() error {
	if s.EmbedDimension <= 0 {
		// Embeddings disabled (tests, minimal setups). Nothing to check.
		return nil
	}
	for _, table := range vecTables {
		existing, err := vecTableDimension(s.db, table)
		if err != nil {
			return err
		}
		if existing != 0 && existing != s.EmbedDimension {
			return &VecDimensionMismatchError{
				Table:      table,
				Existing:   existing,
				Configured: s.EmbedDimension,
			}
		}
	}
	return nil
}

// VecIndexDimensions reports the on-disk dimension of every known vector table,
// keyed by table name. A table that does not exist yet maps to 0. Used by
// `her reembed` to show what it is about to replace.
func (s *SQLiteStore) VecIndexDimensions() (map[string]int, error) {
	dims := make(map[string]int, len(vecTables))
	for _, table := range vecTables {
		dim, err := vecTableDimension(s.db, table)
		if err != nil {
			return nil, err
		}
		dims[table] = dim
	}
	return dims, nil
}

// RebuildVecIndex drops and recreates both sqlite-vec virtual tables at
// s.EmbedDimension. The BLOB columns on `memories` and `mood_entries` are
// deliberately left alone: they are the derived caches ReembedAll overwrites,
// and a half-finished rebuild should degrade to "no obvious duplicates"
// (incompatible vectors score 0) rather than to a second silent corruption.
//
// SQLite cannot drop a row from a virtual table's schema in place, so this is
// DROP + CREATE. Both statements run in one transaction so a crash between them
// cannot leave the database without an index.
func (s *SQLiteStore) RebuildVecIndex() error {
	if s.EmbedDimension <= 0 {
		return fmt.Errorf("rebuilding vector index: embed dimension is 0 (embeddings disabled)")
	}
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("rebuilding vector index: %w", err)
	}
	// Rollback is a no-op once Commit succeeds, so this is safe to defer
	// unconditionally — the usual Go transaction idiom.
	defer tx.Rollback()

	for _, table := range vecTables {
		if _, err := tx.Exec(`DROP TABLE IF EXISTS ` + table); err != nil {
			return fmt.Errorf("dropping %s: %w", table, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(
			`CREATE VIRTUAL TABLE %s USING vec0(embedding float[%d] distance_metric=cosine)`,
			table, s.EmbedDimension,
		)); err != nil {
			return fmt.Errorf("recreating %s at %d dimensions: %w", table, s.EmbedDimension, err)
		}
	}
	return tx.Commit()
}

// Embedder is the one method ReembedAll needs from an embedding client.
//
// Go interfaces are satisfied implicitly: *embed.Client already has this
// method, so it is usable here with no declaration anywhere. Declaring the
// narrowest interface at the point of use (rather than importing the concrete
// client) is what makes ReembedAll testable without a network — the test passes
// a closure.
type Embedder interface {
	Embed(text string) ([]float32, error)
}

// ReembedOptions tunes ReembedAll.
type ReembedOptions struct {
	// Progress, when set, is called after each successfully re-embedded row
	// with a human-readable line. The CLI prints it; tests collect it.
	Progress func(line string)
	// EmbedMoodNotes controls whether mood entries are re-embedded too.
	// Defaults true through the CLI; tests may turn it off for speed.
	SkipMoods bool
}

// ReembedStats counts what ReembedAll did, for the CLI summary and the log.
type ReembedStats struct {
	Memories      int // memories whose vectors were rewritten
	Moods         int // mood entries whose vectors were rewritten
	Failed        int // rows skipped because embedding failed
	LastMemoryID  int64
	LastMoodID    int64
	TotalMemories int // rows found before starting
	TotalMoods    int
}

// ReembedAll recomputes every stored vector from the row's text using e, and
// rewrites both the BLOB cache and the vec_* mirror.
//
// It is idempotent and resumable: each row is updated independently, so
// re-running after a crash simply recomputes the vectors again. Failures are
// counted and skipped — one bad row (empty text, provider hiccup) must not
// abandon a multi-thousand-row backfill halfway.
//
// Call RebuildVecIndex first, otherwise the vec_* tables are still sized for the
// old model and every mirror insert fails.
func (s *SQLiteStore) ReembedAll(e Embedder, opts ReembedOptions) (ReembedStats, error) {
	var stats ReembedStats

	if s.EmbedDimension <= 0 {
		return stats, fmt.Errorf("reembed: embed dimension is 0 (embeddings disabled)")
	}

	// Memories: two vectors each. `embedding` is built from the tags (what
	// vec_memories is searched with), `embedding_text` from the content (used
	// for text-similarity duplicate checks). See tools/memgate/pipeline.go for
	// the write path this mirrors.
	memories, err := s.AllActiveMemories()
	if err != nil {
		return stats, fmt.Errorf("reembed: loading memories: %w", err)
	}
	stats.TotalMemories = len(memories)

	for _, m := range memories {
		// Tag vector input: tags when present, else the content — the same
		// fallback the write path uses (`if embedText == "" { embedText = input.Text }`).
		tagText := m.Tags
		if tagText == "" {
			tagText = m.Content
		}
		tagVec, err := e.Embed(tagText)
		if err != nil {
			stats.Failed++
			continue
		}

		// Text vector input: content plus optional context.
		textInput := m.Content
		if m.Context != "" {
			textInput = m.Content + " " + m.Context
		}
		textVec, err := e.Embed(textInput)
		if err != nil {
			stats.Failed++
			continue
		}

		if err := s.UpdateMemoryEmbedding(m.ID, tagVec, textVec); err != nil {
			stats.Failed++
			continue
		}
		stats.Memories++
		stats.LastMemoryID = m.ID
		if opts.Progress != nil {
			opts.Progress(fmt.Sprintf("memory #%d re-embedded", m.ID))
		}
	}

	if opts.SkipMoods {
		return stats, nil
	}

	// Moods: one vector per entry, built from note + labels — the same string
	// mood/agent.go embeds at write time, so dedup geometry is preserved.
	moods, err := s.allMoodEntries()
	if err != nil {
		return stats, fmt.Errorf("reembed: loading moods: %w", err)
	}
	stats.TotalMoods = len(moods)

	for _, entry := range moods {
		embedText := entry.Note
		if len(entry.Labels) > 0 {
			embedText = embedText + " " + strings.Join(entry.Labels, " ")
		}
		if strings.TrimSpace(embedText) == "" {
			stats.Failed++
			continue
		}
		vec, err := e.Embed(embedText)
		if err != nil {
			stats.Failed++
			continue
		}
		entry.Embedding = vec
		if err := s.UpdateMoodEntry(entry.ID, &entry); err != nil {
			stats.Failed++
			continue
		}
		stats.Moods++
		stats.LastMoodID = entry.ID
		if opts.Progress != nil {
			opts.Progress(fmt.Sprintf("mood #%d re-embedded", entry.ID))
		}
	}

	return stats, nil
}

// allMoodEntries returns every non-superseded mood entry, oldest first.
//
// MoodEntriesInRange is the existing public reader but it takes a window; for a
// full backfill the window is "everything", so the query is written out here
// rather than passing sentinel timestamps. scanMoodEntries is reused so the
// column order stays in one place.
func (s *SQLiteStore) allMoodEntries() ([]MoodEntry, error) {
	rows, err := s.db.Query(
		`SELECT id, ts, kind, valence, labels, associations, note, source,
		        confidence, conversation_id, embedding, created_at,
		        superseded_by, supersede_reason
		 FROM mood_entries
		 WHERE superseded_by IS NULL
		 ORDER BY ts ASC`,
	)
	if err != nil {
		return nil, fmt.Errorf("querying mood entries: %w", err)
	}
	defer rows.Close()
	return scanMoodEntries(rows)
}
