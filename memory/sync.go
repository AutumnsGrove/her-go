package memory

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"her/d1"

	"golang.org/x/sync/errgroup"
)

// ---------------------------------------------------------------------------
// Pull — the "read" half of D1 sync
// ---------------------------------------------------------------------------
//
// Pull fetches rows from Cloudflare D1 that this machine hasn't seen yet
// and upserts them into local SQLite. It's the counterpart to the carrier
// goroutine (synced_store.go) which pushes local writes to D1.
//
// Two flavors of table pull:
//   - Incremental: tables with an auto-increment id column. We track
//     last_synced_id per table in _sync_meta and only fetch rows beyond it.
//   - Full: tables with composite keys (memory_links) or singletons
//     (persona_state). We pull everything and INSERT OR REPLACE.

const (
	// pullPageSize is how many rows to fetch from D1 per query.
	// D1 has response size limits; 500 rows keeps us well within the
	// 5 MB response cap for our schema widths.
	pullPageSize = 500
)

// incrementalTables have an auto-increment id column. Pull uses
// WHERE id > last_synced_id to fetch only new rows.
var incrementalTables = []string{
	"messages",
	"summaries",
	"memories",
	"reflections",
	"persona_versions",
	"traits",
	"mood_entries",
}

// fullPullTables are pulled in their entirety on every sync. These
// either have composite primary keys (memory_links) or are singletons
// (persona_state), so incremental tracking doesn't apply.
var fullPullTables = []string{
	"memory_links",
	"persona_state",
}

// ---------------------------------------------------------------------------
// Pull entry point
// ---------------------------------------------------------------------------

// Pull fetches new rows from D1 and upserts them into local SQLite.
// It runs concurrent per-table pulls, then bumps sqlite_sequence for
// each incremental table to prevent ID collisions when this machine
// resumes writing.
//
// Pull does NOT handle embedding backfill — the caller should call
// MemoriesWithoutEmbeddings() after Pull returns and embed any new
// memories. This keeps the sync layer free of embedding dependencies.
//
// Safe to call multiple times; each call is idempotent. On the first
// call it creates the local _sync_meta table (same shape as D1's copy).
func (s *SyncedStore) Pull(ctx context.Context) error {
	if err := s.initSyncMeta(); err != nil {
		return fmt.Errorf("initializing sync meta: %w", err)
	}

	// Disable FK checks for the duration of the pull. Tables are pulled
	// concurrently, so child rows (e.g. memories with source_message_id)
	// may arrive before their parent rows (messages). This is safe
	// because the data in D1 already passed FK validation when it was
	// first written — we're replicating, not creating relationships.
	//
	// PRAGMA foreign_keys is per-connection in SQLite, so this only
	// affects the pull's upserts, not concurrent bot writes.
	s.SQLiteStore.db.Exec("PRAGMA foreign_keys = OFF")
	defer s.SQLiteStore.db.Exec("PRAGMA foreign_keys = ON")

	// Use a plain errgroup (not WithContext) so one table's failure
	// doesn't cancel the others — we want best-effort for each table.
	// This is like asyncio.gather(return_exceptions=False) in Python,
	// except errgroup still waits for all goroutines before returning
	// the first error.
	var g errgroup.Group

	for _, table := range incrementalTables {
		table := table // capture for goroutine — fixed in Go 1.22 but safe either way
		g.Go(func() error {
			return s.pullIncremental(ctx, table)
		})
	}

	for _, table := range fullPullTables {
		table := table
		g.Go(func() error {
			return s.pullFull(ctx, table)
		})
	}

	if err := g.Wait(); err != nil {
		return fmt.Errorf("pulling from D1: %w", err)
	}

	log.Info("d1 pull complete")
	return nil
}

// ---------------------------------------------------------------------------
// Incremental pull (tables with auto-increment id)
// ---------------------------------------------------------------------------

// pullIncremental fetches rows from D1 where id > last_synced_id,
// upserts them into local SQLite, bumps sqlite_sequence, and updates
// _sync_meta. Uses pagination so large tables (like messages) don't
// blow up D1's response size limit.
func (s *SyncedStore) pullIncremental(ctx context.Context, table string) error {
	spec, ok := syncedTableSpecs[table]
	if !ok {
		return fmt.Errorf("no table spec for %q", table)
	}

	lastID, err := s.getLastSyncedID(table)
	if err != nil {
		return fmt.Errorf("getting last synced ID for %s: %w", table, err)
	}

	var totalPulled int
	cursor := lastID

	// Paginated pull — keep fetching pages until we get fewer rows
	// than pullPageSize, which means we've hit the end.
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		query := fmt.Sprintf(
			"SELECT %s FROM %s WHERE id > ? ORDER BY id LIMIT ?",
			spec.d1Cols, table,
		)
		result, err := s.d1Client.Query(query, cursor, pullPageSize)
		if err != nil {
			return fmt.Errorf("querying D1 for %s (cursor=%d): %w", table, cursor, err)
		}

		if len(result.Results) == 0 {
			break
		}

		if err := s.upsertRows(table, spec, result.Results); err != nil {
			return fmt.Errorf("upserting %s rows: %w", table, err)
		}

		// Advance cursor to the highest ID in this page.
		pageMax := extractMaxID(result.Results, "id")
		if pageMax > cursor {
			cursor = pageMax
		}
		totalPulled += len(result.Results)

		if len(result.Results) < pullPageSize {
			break // last page
		}
	}

	if totalPulled > 0 {
		// Update _sync_meta so the next pull skips these rows.
		if err := s.setLastSyncedID(table, cursor); err != nil {
			return fmt.Errorf("updating sync meta for %s: %w", table, err)
		}

		// Bump sqlite_sequence so the next INSERT on this machine
		// picks up after the highest D1 ID. Without this, the next
		// local write could reuse an ID that the other machine created.
		if err := s.bumpSequence(table, cursor); err != nil {
			log.Warn("failed to bump sqlite_sequence", "table", table, "err", err)
			// Non-fatal — the data is synced, sequence bump is a safety net.
		}

		log.Info("d1 pulled", "table", table, "rows", totalPulled, "max_id", cursor)
	}

	return nil
}

// ---------------------------------------------------------------------------
// Full pull (composite-key and singleton tables)
// ---------------------------------------------------------------------------

// pullFull fetches ALL rows from a table and upserts them into local
// SQLite. Used for tables where incremental tracking doesn't make sense:
//   - memory_links has a composite PK (source_id, target_id)
//   - persona_state is a singleton row (id always 1)
func (s *SyncedStore) pullFull(ctx context.Context, table string) error {
	spec, ok := syncedTableSpecs[table]
	if !ok {
		return fmt.Errorf("no table spec for %q", table)
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	query := fmt.Sprintf("SELECT %s FROM %s", spec.d1Cols, table)
	result, err := s.d1Client.Query(query)
	if err != nil {
		return fmt.Errorf("querying D1 for %s: %w", table, err)
	}

	if len(result.Results) == 0 {
		return nil
	}

	if err := s.upsertRows(table, spec, result.Results); err != nil {
		return fmt.Errorf("upserting %s rows: %w", table, err)
	}

	log.Info("d1 pulled", "table", table, "rows", len(result.Results))
	return nil
}

// ---------------------------------------------------------------------------
// Local SQLite upsert
// ---------------------------------------------------------------------------

// upsertRows INSERT OR REPLACEs a batch of D1 rows into local SQLite.
// Runs inside a transaction for atomicity and uses a prepared statement
// for performance — same pattern as bulk inserts in Python with
// cursor.executemany(), but explicit.
func (s *SyncedStore) upsertRows(table string, spec tableSpec, rows []d1.Row) error {
	query := fmt.Sprintf(
		"INSERT OR REPLACE INTO %s (%s) VALUES (%s)",
		table, spec.d1Cols, spec.placeholders,
	)

	// Wrap in a transaction — if one row fails, none are committed.
	// Also much faster than individual INSERTs: SQLite commits once
	// instead of once per row (each commit is an fsync to disk).
	tx, err := s.SQLiteStore.db.Begin()
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback() // no-op if Commit() succeeds

	// Prepared statements inside a transaction are a Go/SQL best
	// practice for bulk inserts. The driver parses the SQL once,
	// then re-binds parameters for each row. In Python terms, it's
	// like cursor.executemany(sql, rows) — same idea, more explicit.
	prepared, err := tx.Prepare(query)
	if err != nil {
		return fmt.Errorf("preparing upsert for %s: %w", table, err)
	}
	defer prepared.Close()

	cols := splitCols(spec.d1Cols)

	for _, row := range rows {
		params := rowToParams(row, cols)
		if _, err := prepared.Exec(params...); err != nil {
			return fmt.Errorf("inserting row into %s: %w", table, err)
		}
	}

	return tx.Commit()
}

// ---------------------------------------------------------------------------
// _sync_meta helpers
// ---------------------------------------------------------------------------

// initSyncMeta creates the local _sync_meta table if it doesn't exist.
// This mirrors the _sync_meta table in D1 (created by d1/schema.sql).
// Each row tracks how far this machine has synced for a given table.
func (s *SyncedStore) initSyncMeta() error {
	_, err := s.SQLiteStore.db.Exec(`CREATE TABLE IF NOT EXISTS _sync_meta (
		table_name     TEXT PRIMARY KEY,
		last_synced_id INTEGER NOT NULL DEFAULT 0
	)`)
	return err
}

// getLastSyncedID reads the last synced row ID for a table from local
// _sync_meta. Returns 0 if no entry exists yet (first sync for this table).
func (s *SyncedStore) getLastSyncedID(table string) (int64, error) {
	var id int64
	err := s.SQLiteStore.db.QueryRow(
		"SELECT last_synced_id FROM _sync_meta WHERE table_name = ?", table,
	).Scan(&id)
	if err != nil {
		// sql.ErrNoRows means first sync — start from 0.
		// Any other error is also fine to treat as 0 (fail-open).
		return 0, nil
	}
	return id, nil
}

// setLastSyncedID records the highest synced row ID for a table.
// Uses INSERT OR REPLACE so the first call creates the row and
// subsequent calls update it.
func (s *SyncedStore) setLastSyncedID(table string, id int64) error {
	_, err := s.SQLiteStore.db.Exec(
		"INSERT OR REPLACE INTO _sync_meta (table_name, last_synced_id) VALUES (?, ?)",
		table, id,
	)
	return err
}

// ---------------------------------------------------------------------------
// sqlite_sequence management
// ---------------------------------------------------------------------------

// bumpSequence ensures the local sqlite_sequence for a table is at least
// maxID. This prevents ID collisions: if the other machine created rows
// up to ID 500, this machine's next INSERT should start at 501+.
//
// sqlite_sequence is an internal SQLite table that tracks the next
// AUTOINCREMENT value. It only exists for tables that use AUTOINCREMENT
// (which all our synced tables do). The MAX() call means we never
// decrease the sequence — only bump it up if D1 has a higher ID.
func (s *SyncedStore) bumpSequence(table string, maxID int64) error {
	_, err := s.SQLiteStore.db.Exec(
		"UPDATE sqlite_sequence SET seq = MAX(seq, ?) WHERE name = ?",
		maxID, table,
	)
	return err
}

// ---------------------------------------------------------------------------
// D1 row → SQLite parameter conversion
// ---------------------------------------------------------------------------

// rowToParams extracts values from a D1 row (map[string]any) in the
// column order expected by the prepared INSERT statement. D1 returns
// JSON types (float64 for numbers, string, bool, nil for NULL), so
// we normalize them to types the Go SQLite driver understands.
func rowToParams(row d1.Row, cols []string) []any {
	params := make([]any, len(cols))
	for i, col := range cols {
		params[i] = normalizeD1Value(row[col])
	}
	return params
}

// normalizeD1Value converts D1's JSON-decoded types to types that
// SQLite's Go driver handles natively. The main conversion is
// float64 → int64 for whole numbers (IDs, counts, booleans).
//
// D1 uses plain json.Unmarshal, which decodes all JSON numbers as
// float64 when the target is any/interface{}. This is fine for REAL
// columns (similarity, confidence) but we convert whole-number
// floats to int64 for cleanliness in INTEGER/BOOLEAN columns.
func normalizeD1Value(v any) any {
	switch val := v.(type) {
	case float64:
		// Whole number → int64. Covers IDs, counts, booleans (0/1),
		// importance scores, etc. float64 can represent integers up
		// to 2^53 exactly, so no precision loss for our ID ranges.
		if val == float64(int64(val)) {
			return int64(val)
		}
		return val
	case json.Number:
		// Safety net: if the D1 client ever switches to UseNumber().
		if i, err := val.Int64(); err == nil {
			return i
		}
		if f, err := val.Float64(); err == nil {
			return f
		}
		return val.String()
	default:
		// string, bool, nil (NULL) — pass through unchanged.
		return v
	}
}

// extractMaxID finds the maximum value of the given column across
// a slice of D1 result rows. Used to track the cursor position
// during paginated pulls.
func extractMaxID(rows []d1.Row, col string) int64 {
	var max int64
	for _, row := range rows {
		if v, ok := toInt64(row[col]); ok && v > max {
			max = v
		}
	}
	return max
}

// toInt64 converts a D1 value (typically float64 from JSON) to int64.
func toInt64(v any) (int64, bool) {
	switch val := v.(type) {
	case float64:
		return int64(val), true
	case int64:
		return val, true
	case json.Number:
		i, err := val.Int64()
		return i, err == nil
	default:
		return 0, false
	}
}

// splitCols splits a comma-separated column string into trimmed names.
// "id, timestamp, role" → ["id", "timestamp", "role"]
func splitCols(cols string) []string {
	parts := strings.Split(cols, ",")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	return parts
}

// ---------------------------------------------------------------------------
// PushAll — bulk upload local SQLite → D1 (initial seeding)
// ---------------------------------------------------------------------------

// PushAll uploads all rows from every synced table in local SQLite to D1.
// This is the initial seeding operation: run it once on the machine that
// has all the real data to populate D1 for the first time.
//
// Idempotent: uses INSERT OR REPLACE, so running it twice is safe.
// Concurrent per-table uploads for speed, with batches of maxBatchSize
// rows per D1 API call.
//
// After pushing, updates local _sync_meta so this machine knows it's
// fully synced (future Pulls won't re-download what we just pushed).
func (s *SyncedStore) PushAll(ctx context.Context) error {
	if err := s.initSyncMeta(); err != nil {
		return fmt.Errorf("initializing sync meta: %w", err)
	}

	allTables := make([]string, 0, len(incrementalTables)+len(fullPullTables))
	allTables = append(allTables, incrementalTables...)
	allTables = append(allTables, fullPullTables...)

	// Push tables concurrently — each table is independent, and D1
	// handles concurrent HTTP requests fine. errgroup collects the
	// first error but still waits for all goroutines to finish.
	var g errgroup.Group
	for _, table := range allTables {
		table := table
		g.Go(func() error {
			return s.pushTable(ctx, table)
		})
	}

	if err := g.Wait(); err != nil {
		return fmt.Errorf("pushing to D1: %w", err)
	}

	log.Info("d1 push complete")
	return nil
}

// pushTable reads all rows from a local SQLite table and pushes them
// to D1 in batches. Logs progress as it goes. After pushing, updates
// _sync_meta for incremental tables.
func (s *SyncedStore) pushTable(ctx context.Context, table string) error {
	spec, ok := syncedTableSpecs[table]
	if !ok {
		return fmt.Errorf("no table spec for %q", table)
	}

	// Count rows for progress logging.
	var total int
	if err := s.SQLiteStore.db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&total); err != nil {
		return fmt.Errorf("counting %s rows: %w", table, err)
	}
	if total == 0 {
		log.Info("d1 push", "table", table, "rows", "0/0")
		return nil
	}

	// Determine ORDER BY — most tables use id, memory_links uses composite PK.
	orderBy := "id"
	if table == "memory_links" {
		orderBy = "source_id, target_id"
	}

	// Stream rows from local SQLite. We don't load everything into memory —
	// just scan one row at a time and accumulate batches.
	rows, err := s.SQLiteStore.db.QueryContext(ctx,
		fmt.Sprintf("SELECT %s FROM %s ORDER BY %s", spec.selectCols, table, orderBy),
	)
	if err != nil {
		return fmt.Errorf("reading %s: %w", table, err)
	}
	defer rows.Close()

	colCount := strings.Count(spec.d1Cols, ",") + 1
	insertSQL := fmt.Sprintf("INSERT OR REPLACE INTO %s (%s) VALUES (%s)",
		table, spec.d1Cols, spec.placeholders)

	var batch []d1.Statement
	var pushed int
	var maxID int64

	for rows.Next() {
		// Scan row values. Same pointer-indirection pattern as readRow/scanRow,
		// but for sql.Rows instead of sql.Row.
		values := make([]any, colCount)
		ptrs := make([]any, colCount)
		for i := range values {
			ptrs[i] = &values[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return fmt.Errorf("scanning %s row: %w", table, err)
		}

		// Track max ID for _sync_meta (first column is id for incremental tables).
		if id, ok := toInt64(values[0]); ok && id > maxID {
			maxID = id
		}

		batch = append(batch, d1.Statement{SQL: insertSQL, Params: values})

		if len(batch) >= maxBatchSize {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if _, err := s.d1Client.Batch(batch); err != nil {
				return fmt.Errorf("pushing %s batch: %w", table, err)
			}
			pushed += len(batch)
			log.Info("d1 push", "table", table, "progress", fmt.Sprintf("%d/%d", pushed, total))
			batch = batch[:0]
		}
	}

	if err := rows.Err(); err != nil {
		return fmt.Errorf("iterating %s rows: %w", table, err)
	}

	// Push remaining rows in the last partial batch.
	if len(batch) > 0 {
		if _, err := s.d1Client.Batch(batch); err != nil {
			return fmt.Errorf("pushing %s final batch: %w", table, err)
		}
		pushed += len(batch)
	}

	log.Info("d1 push done", "table", table, "rows", fmt.Sprintf("%d/%d", pushed, total))

	// Update _sync_meta for incremental tables so future Pulls on this
	// machine skip rows we already have locally.
	if isIncremental(table) && maxID > 0 {
		if err := s.setLastSyncedID(table, maxID); err != nil {
			return fmt.Errorf("updating sync meta for %s: %w", table, err)
		}
	}

	return nil
}

// isIncremental returns true if the table uses ID-based incremental sync.
func isIncremental(table string) bool {
	for _, t := range incrementalTables {
		if t == table {
			return true
		}
	}
	return false
}
