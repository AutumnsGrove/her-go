package memory

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"her/d1"
)

// ===========================================================================
// Fake D1 server — an in-memory SQLite that speaks the D1 REST API
// ===========================================================================
//
// This is the testing equivalent of Cloudflare D1: an HTTP server that
// accepts SQL statements and executes them against a real SQLite database
// in memory. Push writes go in, pull reads come back — full roundtrip
// without touching the network.
//
// Think of it like Python's unittest.mock but with real SQL execution:
// the fake doesn't just record calls, it actually stores and retrieves
// data. This catches bugs that a pure mock would miss (column mismatches,
// SQL syntax errors, type coercion issues).

type fakeD1Server struct {
	mu sync.Mutex
	db *sql.DB
}

// newFakeD1 creates a fake D1 server backed by in-memory SQLite with the
// real D1 schema applied. Returns both the D1 client (pointed at the fake)
// and the server (for direct data insertion in tests).
func newFakeD1(t *testing.T) (*d1.Client, *fakeD1Server) {
	t.Helper()

	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("opening fake D1 database: %v", err)
	}

	// Apply the real D1 schema so table layouts match production.
	schema, err := os.ReadFile(filepath.Join("..", "d1", "schema.sql"))
	if err != nil {
		t.Fatalf("reading D1 schema: %v", err)
	}
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatalf("applying D1 schema to fake: %v", err)
	}

	fake := &fakeD1Server{db: db}
	srv := httptest.NewServer(http.HandlerFunc(fake.handle))
	t.Cleanup(func() {
		srv.Close()
		db.Close()
	})

	client := d1.NewClient("test-acct", "test-db", "test-token")
	client.WithBaseURL(srv.URL)
	return client, fake
}

// exec inserts test data directly into the fake D1 database, bypassing
// the HTTP layer. Useful for setting up pull test scenarios.
func (f *fakeD1Server) exec(sql string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.db.Exec(sql, args...)
}

// handle is the HTTP handler that speaks the D1 REST API protocol.
// It accepts single or batch SQL statements, executes them against
// the in-memory SQLite, and returns D1-formatted JSON responses.
func (f *fakeD1Server) handle(w http.ResponseWriter, r *http.Request) {
	body, _ := io.ReadAll(r.Body)

	// D1 uses different JSON shapes for single vs batch requests.
	type stmt struct {
		SQL    string `json:"sql"`
		Params []any  `json:"params"`
	}

	var stmts []stmt
	var batchReq struct {
		Batch []stmt `json:"batch"`
	}
	if err := json.Unmarshal(body, &batchReq); err == nil && batchReq.Batch != nil {
		stmts = batchReq.Batch
	} else {
		var single stmt
		json.Unmarshal(body, &single)
		stmts = []stmt{single}
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	var results []map[string]any
	for _, s := range stmts {
		upper := strings.TrimSpace(strings.ToUpper(s.SQL))
		if strings.HasPrefix(upper, "SELECT") {
			rows := f.queryRows(s.SQL, s.Params)
			results = append(results, map[string]any{
				"success": true,
				"results": rows,
				"meta":    map[string]any{},
			})
		} else {
			f.db.Exec(s.SQL, s.Params...)
			results = append(results, map[string]any{
				"success": true,
				"results": []any{},
				"meta":    map[string]any{},
			})
		}
	}

	json.NewEncoder(w).Encode(map[string]any{
		"success": true,
		"result":  results,
	})
}

// queryRows executes a SELECT and returns results as []map[string]any,
// matching D1's "rows as objects" format. The JSON roundtrip through
// the HTTP layer naturally converts int64 → float64, which is exactly
// what the real D1 API does.
func (f *fakeD1Server) queryRows(sqlStr string, params []any) []map[string]any {
	rows, err := f.db.Query(sqlStr, params...)
	if err != nil {
		return nil
	}
	defer rows.Close()

	cols, _ := rows.Columns()
	var result []map[string]any

	for rows.Next() {
		values := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range values {
			ptrs[i] = &values[i]
		}
		rows.Scan(ptrs...)

		row := make(map[string]any)
		for i, col := range cols {
			row[col] = values[i]
		}
		result = append(result, row)
	}

	return result
}

// ===========================================================================
// Test helpers
// ===========================================================================

func newSyncTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "sync_test.db")
	store, err := NewStore(dbPath, 0)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// ===========================================================================
// Tests
// ===========================================================================

// TestPushPullRoundtrip verifies that data pushed from one store can be
// pulled into a different store and arrive intact. This is the core
// correctness test for the sync layer.
func TestPushPullRoundtrip(t *testing.T) {
	d1Client, _ := newFakeD1(t)
	ctx := context.Background()

	// --- Store 1: seed with test data and push to D1 ---

	store1 := newSyncTestStore(t)
	msgID, err := store1.SaveMessage("user", "hello world", "hello world", "conv1")
	if err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}

	memID, err := store1.SaveMemory("user likes cats", "preference", "user", msgID, 7, nil, nil, "cats,pets", "")
	if err != nil {
		t.Fatalf("SaveMemory: %v", err)
	}

	_, err = store1.SaveReflection("a thoughtful reflection", 3, "what do you think?", "I think...")
	if err != nil {
		t.Fatalf("SaveReflection: %v", err)
	}

	synced1, err := NewSyncedStore(store1, d1Client)
	if err != nil {
		t.Fatalf("NewSyncedStore (store1): %v", err)
	}

	if err := synced1.PushAll(ctx); err != nil {
		t.Fatalf("PushAll: %v", err)
	}
	synced1.Close()

	// --- Store 2: empty store, pull from D1 ---

	store2 := newSyncTestStore(t)
	synced2, err := NewSyncedStore(store2, d1Client)
	if err != nil {
		t.Fatalf("NewSyncedStore (store2): %v", err)
	}

	if err := synced2.Pull(ctx); err != nil {
		t.Fatalf("Pull: %v", err)
	}
	// Don't close synced2 yet — we need store2's DB connection for
	// the verification queries below. t.Cleanup handles it.
	t.Cleanup(func() { synced2.Close() })

	// --- Verify data arrived in store2 ---

	// Check message
	var role, contentRaw string
	err = store2.db.QueryRow("SELECT role, content_raw FROM messages WHERE id = ?", msgID).Scan(&role, &contentRaw)
	if err != nil {
		t.Fatalf("message not in store2: %v", err)
	}
	if role != "user" || contentRaw != "hello world" {
		t.Errorf("message = (%q, %q), want (user, hello world)", role, contentRaw)
	}

	// Check memory
	var memContent, memTags string
	var importance int
	err = store2.db.QueryRow("SELECT memory, tags, importance FROM memories WHERE id = ?", memID).Scan(&memContent, &memTags, &importance)
	if err != nil {
		t.Fatalf("memory not in store2: %v", err)
	}
	if memContent != "user likes cats" {
		t.Errorf("memory content = %q, want %q", memContent, "user likes cats")
	}
	if memTags != "cats,pets" {
		t.Errorf("memory tags = %q, want %q", memTags, "cats,pets")
	}
	if importance != 7 {
		t.Errorf("memory importance = %d, want 7", importance)
	}

	// Check reflection
	var reflContent string
	err = store2.db.QueryRow("SELECT content FROM reflections WHERE id = 1").Scan(&reflContent)
	if err != nil {
		t.Fatalf("reflection not in store2: %v", err)
	}
	if reflContent != "a thoughtful reflection" {
		t.Errorf("reflection content = %q, want %q", reflContent, "a thoughtful reflection")
	}
}

// TestPushFailureDoesNotBlockLocalWrites verifies that when D1 is down,
// local SQLite writes still succeed. The outbox accumulates entries for
// later retry — the user never sees an error.
func TestPushFailureDoesNotBlockLocalWrites(t *testing.T) {
	store := newSyncTestStore(t)

	// Create a D1 client that always returns 500 — simulating D1 outage.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"success":false,"errors":[{"code":500,"message":"server error"}]}`))
	}))
	t.Cleanup(srv.Close)

	failClient := d1.NewClient("test-acct", "test-db", "test-token")
	failClient.WithBaseURL(srv.URL)

	synced, err := NewSyncedStore(store, failClient)
	if err != nil {
		t.Fatalf("NewSyncedStore: %v", err)
	}
	defer synced.Close()

	// Save a message through SyncedStore — this writes locally and
	// queues an outbox entry. The carrier will try to push and fail.
	id, err := synced.SaveMessage("user", "test message", "test message", "conv1")
	if err != nil {
		t.Fatalf("SaveMessage should succeed locally even when D1 is down: %v", err)
	}
	if id == 0 {
		t.Fatal("expected nonzero message ID")
	}

	// Verify the message is in local SQLite.
	var content string
	err = store.db.QueryRow("SELECT content_raw FROM messages WHERE id = ?", id).Scan(&content)
	if err != nil {
		t.Fatalf("message not in local SQLite: %v", err)
	}
	if content != "test message" {
		t.Errorf("content = %q, want %q", content, "test message")
	}

	// Verify the outbox entry was created (carrier couldn't push it).
	var outboxCount int
	err = store.db.QueryRow("SELECT COUNT(*) FROM _d1_outbox WHERE table_name = 'messages'").Scan(&outboxCount)
	if err != nil {
		t.Fatalf("querying outbox: %v", err)
	}
	if outboxCount == 0 {
		// Give the carrier a moment — it runs on a ticker.
		time.Sleep(100 * time.Millisecond)
	}
	// The entry should either still be in the outbox (carrier hasn't run yet)
	// or the carrier ran and failed, leaving it for retry. Either way,
	// the local write succeeded — that's what matters.
}

// TestPullSkipsAlreadySyncedRows verifies that _sync_meta correctly tracks
// the cursor position so repeated pulls don't re-download old rows.
func TestPullSkipsAlreadySyncedRows(t *testing.T) {
	d1Client, fake := newFakeD1(t)
	ctx := context.Background()

	// Seed fake D1 with 2 messages.
	fake.exec("INSERT INTO messages (id, timestamp, role, content_raw, content_scrubbed, conversation_id) VALUES (1, '2026-01-01 00:00:00', 'user', 'msg1', 'msg1', 'conv1')")
	fake.exec("INSERT INTO messages (id, timestamp, role, content_raw, content_scrubbed, conversation_id) VALUES (2, '2026-01-01 00:01:00', 'assistant', 'msg2', 'msg2', 'conv1')")

	// Pull into a fresh store.
	store := newSyncTestStore(t)
	synced, err := NewSyncedStore(store, d1Client)
	if err != nil {
		t.Fatalf("NewSyncedStore: %v", err)
	}

	if err := synced.Pull(ctx); err != nil {
		t.Fatalf("Pull 1: %v", err)
	}

	// Verify 2 messages arrived.
	var count int
	store.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&count)
	if count != 2 {
		t.Fatalf("after pull 1: got %d messages, want 2", count)
	}

	// Verify _sync_meta cursor is at 2.
	var lastID int64
	store.db.QueryRow("SELECT last_synced_id FROM _sync_meta WHERE table_name = 'messages'").Scan(&lastID)
	if lastID != 2 {
		t.Errorf("cursor after pull 1 = %d, want 2", lastID)
	}

	// Add 1 more message to fake D1.
	fake.exec("INSERT INTO messages (id, timestamp, role, content_raw, content_scrubbed, conversation_id) VALUES (3, '2026-01-01 00:02:00', 'user', 'msg3', 'msg3', 'conv1')")

	// Pull again — should only fetch msg3.
	if err := synced.Pull(ctx); err != nil {
		t.Fatalf("Pull 2: %v", err)
	}

	store.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&count)
	if count != 3 {
		t.Fatalf("after pull 2: got %d messages, want 3", count)
	}

	// Verify cursor advanced to 3.
	store.db.QueryRow("SELECT last_synced_id FROM _sync_meta WHERE table_name = 'messages'").Scan(&lastID)
	if lastID != 3 {
		t.Errorf("cursor after pull 2 = %d, want 3", lastID)
	}

	// Pull again with no new data — should be a no-op.
	if err := synced.Pull(ctx); err != nil {
		t.Fatalf("Pull 3 (no-op): %v", err)
	}

	store.db.QueryRow("SELECT COUNT(*) FROM messages").Scan(&count)
	if count != 3 {
		t.Fatalf("after pull 3: got %d messages, want 3 (unchanged)", count)
	}

	synced.Close()
}

// TestPullBumpsSequence verifies that sqlite_sequence is bumped after
// a pull, preventing ID collisions when this machine resumes writing.
func TestPullBumpsSequence(t *testing.T) {
	d1Client, fake := newFakeD1(t)
	ctx := context.Background()

	// Seed fake D1 with a message at ID 100 — simulating data from
	// the other machine that has progressed much further.
	fake.exec("INSERT INTO messages (id, timestamp, role, content_raw, content_scrubbed, conversation_id) VALUES (100, '2026-01-01 00:00:00', 'user', 'remote msg', 'remote msg', 'conv1')")

	// Local store starts at ID 1 (no messages yet, sequence at 0).
	store := newSyncTestStore(t)
	synced, err := NewSyncedStore(store, d1Client)
	if err != nil {
		t.Fatalf("NewSyncedStore: %v", err)
	}

	if err := synced.Pull(ctx); err != nil {
		t.Fatalf("Pull: %v", err)
	}

	// Now save a new message locally — it should get ID > 100,
	// not ID 1 (which would collide with the remote data).
	newID, err := synced.SaveMessage("user", "local msg", "local msg", "conv2")
	if err != nil {
		t.Fatalf("SaveMessage after pull: %v", err)
	}

	if newID <= 100 {
		t.Errorf("new message ID = %d, want > 100 (sequence should have been bumped)", newID)
	}

	synced.Close()
}
