package memory

import (
	"context"
	"database/sql"
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

// Default pull page size — used when config omits the sync section.
const defaultPullPageSize = 500

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

// Pull phases — tables are grouped by FK dependencies so parent rows
// exist before child rows are inserted. Tables within a phase run
// concurrently; phases run sequentially.
//
//	Phase 1: no FK deps on other synced tables
//	Phase 2: memories → messages, traits → persona_versions
//	Phase 3: memory_links → memories
var pullPhases = [][]string{
	{"messages", "summaries", "reflections", "persona_versions", "mood_entries", "persona_state"},
	{"memories", "traits"},
	{"memory_links"},
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

	// Pull tables in FK-dependency order. Each phase runs its tables
	// concurrently, but phases are sequential — so parent rows (messages)
	// exist before child rows (memories) are inserted.
	for _, phase := range pullPhases {
		var g errgroup.Group

		for _, table := range phase {
			table := table
			g.Go(func() error {
				if isIncremental(table) {
					return s.pullIncremental(ctx, table)
				}
				return s.pullFull(ctx, table)
			})
		}

		if err := g.Wait(); err != nil {
			return fmt.Errorf("pulling from D1: %w", err)
		}
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
	// than s.PullPageSize, which means we've hit the end.
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		query := fmt.Sprintf(
			"SELECT %s FROM %s WHERE id > ? ORDER BY id LIMIT ?",
			spec.d1Cols, table,
		)
		result, err := s.d1Client.Query(query, cursor, s.PullPageSize)
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

		if len(result.Results) < s.PullPageSize {
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
//
// Uses a pinned connection (db.Conn) with PRAGMA foreign_keys = OFF to
// handle self-referential FKs (e.g. memories.superseded_by → memories.id)
// where a row may reference another row in the same batch. The phase
// ordering in Pull handles cross-table FKs; this handles intra-table ones.
//
// Why db.Conn? Go's database/sql uses a connection pool. db.Exec("PRAGMA ...")
// and db.Begin() can land on different pooled connections, so a PRAGMA set
// on one connection has no effect on transactions opened from the pool.
// db.Conn pins a single underlying connection, guaranteeing the PRAGMA and
// the transaction share it — like Python's `with conn:` block.
func (s *SyncedStore) upsertRows(table string, spec tableSpec, rows []d1.Row) error {
	ctx := context.Background()

	// Pin a single connection so the FK pragma and transaction share it.
	conn, err := s.SQLiteStore.db.Conn(ctx)
	if err != nil {
		return fmt.Errorf("pinning connection: %w", err)
	}
	defer conn.Close()

	// Disable FK checks on this connection. Safe because the data already
	// passed FK validation in D1 — we're replicating, not creating relationships.
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		return fmt.Errorf("disabling foreign keys: %w", err)
	}
	defer conn.ExecContext(ctx, "PRAGMA foreign_keys = ON")

	query := fmt.Sprintf(
		"INSERT OR REPLACE INTO %s (%s) VALUES (%s)",
		table, spec.d1Cols, spec.placeholders,
	)

	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("beginning transaction: %w", err)
	}
	defer tx.Rollback()

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
	if err == sql.ErrNoRows {
		return 0, nil // first sync for this table — start from 0
	}
	if err != nil {
		// Unexpected error (corrupt table, etc.). Fail-open to 0 so we
		// re-pull everything (idempotent upserts make this safe), but log
		// it so silent full re-pulls are observable.
		log.Warn("reading _sync_meta, falling back to full pull", "table", table, "err", err)
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
// Concurrent per-table uploads for speed, with batches of BatchSize
// rows per D1 API call.
//
// After pushing, updates local _sync_meta so this machine knows it's
// fully synced (future Pulls won't re-download what we just pushed).
func (s *SyncedStore) PushAll(ctx context.Context) error {
	if err := s.initSyncMeta(); err != nil {
		return fmt.Errorf("initializing sync meta: %w", err)
	}

	// Flush any pending outbox entries first. These represent updates to
	// existing rows (e.g. importance score changes on memory #52) that the
	// carrier hasn't pushed yet. Without this, the incremental push below
	// would skip those rows because their IDs are already in D1.
	flushed, err := s.FlushOutbox()
	if err != nil {
		log.Warn("flushing outbox before push", "err", err)
	} else if flushed > 0 {
		log.Info("d1 outbox flushed before push", "entries", flushed)
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

// pushTable reads rows from a local SQLite table and pushes them to D1
// in batches. For incremental tables (those with an auto-increment id),
// it first queries D1 for MAX(id) and only pushes rows beyond that —
// so re-running `sync push` is cheap when D1 is already up to date.
// Full-pull tables (composite keys, singletons) are always re-sent.
//
// After pushing, updates _sync_meta for incremental tables.
func (s *SyncedStore) pushTable(ctx context.Context, table string) error {
	spec, ok := syncedTableSpecs[table]
	if !ok {
		return fmt.Errorf("no table spec for %q", table)
	}

	// Determine ORDER BY and WHERE clause. For incremental tables, ask D1
	// what it already has and skip those rows. This makes re-runs fast —
	// like Python's "if not exists" guard, but at the row level.
	orderBy := "id"
	whereClause := ""
	var queryArgs []any

	if table == "memory_links" {
		orderBy = "source_id, target_id"
	} else if isIncremental(table) {
		remoteMax, err := s.getRemoteMaxID(ctx, table)
		if err != nil {
			log.Warn("could not query D1 max id, pushing all rows", "table", table, "err", err)
		} else if remoteMax > 0 {
			whereClause = " WHERE id > ?"
			queryArgs = append(queryArgs, remoteMax)
		}
	}

	// Count rows that need pushing (not total rows).
	var total int
	countQuery := "SELECT COUNT(*) FROM " + table + whereClause
	if err := s.SQLiteStore.db.QueryRow(countQuery, queryArgs...).Scan(&total); err != nil {
		return fmt.Errorf("counting %s rows: %w", table, err)
	}
	if total == 0 {
		log.Info("d1 push", "table", table, "status", "already up to date")
		return nil
	}

	// Stream rows from local SQLite. We don't load everything into memory —
	// just scan one row at a time and accumulate batches.
	selectQuery := fmt.Sprintf("SELECT %s FROM %s%s ORDER BY %s",
		spec.selectCols, table, whereClause, orderBy)
	rows, err := s.SQLiteStore.db.QueryContext(ctx, selectQuery, queryArgs...)
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

		if len(batch) >= s.BatchSize {
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

// getRemoteMaxID queries D1 for the highest id in a table. Returns 0
// if the table is empty or doesn't exist yet. Used by pushTable to
// skip rows D1 already has — a single cheap query that can save
// hundreds of INSERT OR REPLACE round-trips.
func (s *SyncedStore) getRemoteMaxID(ctx context.Context, table string) (int64, error) {
	result, err := s.d1Client.Query(
		fmt.Sprintf("SELECT COALESCE(MAX(id), 0) AS max_id FROM %s", table),
	)
	if err != nil {
		return 0, err
	}
	if len(result.Results) == 0 {
		return 0, nil
	}
	id, ok := toInt64(result.Results[0]["max_id"])
	if !ok {
		return 0, nil
	}
	return id, nil
}
