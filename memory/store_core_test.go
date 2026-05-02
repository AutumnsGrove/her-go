package memory

import (
	"encoding/json"
	"path/filepath"
	"testing"
)

// newCoreTestStore creates a fresh temp store for testing. Used by
// summaries, PII vault, metrics, agent turns, and confirmations tests.
func newCoreTestStore(t *testing.T) *SQLiteStore {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "core_test.db")
	store, err := NewStore(dbPath, 0)
	if err != nil {
		t.Fatalf("NewStore: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// =====================================================================
// Store initialization
// =====================================================================

func TestNewStore_CreatesDB(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "brand_new.db")
	store, err := NewStore(dbPath, 0)
	if err != nil {
		t.Fatalf("NewStore on fresh path: %v", err)
	}
	defer store.Close()

	// Verify the DB is usable — insert and query a message
	id, err := store.SaveMessage("user", "hello", "", "test")
	if err != nil {
		t.Fatalf("SaveMessage on fresh DB: %v", err)
	}
	if id == 0 {
		t.Fatal("expected nonzero message ID")
	}
}

func TestNewStore_WithVecDimension(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "vec.db")
	store, err := NewStore(dbPath, 768)
	if err != nil {
		t.Fatalf("NewStore with vec dimension: %v", err)
	}
	defer store.Close()

	if store.EmbedDimension != 768 {
		t.Errorf("EmbedDimension = %d, want 768", store.EmbedDimension)
	}

	// vec_memories should exist and be queryable
	count, err := store.VecMemoriesCount()
	if err != nil {
		t.Fatalf("VecMemoriesCount: %v", err)
	}
	if count != 0 {
		t.Errorf("vec_memories count = %d, want 0 on fresh DB", count)
	}
}

func TestNewStore_IdempotentInit(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "idem.db")

	// Open, create tables, close
	store1, err := NewStore(dbPath, 0)
	if err != nil {
		t.Fatalf("first open: %v", err)
	}
	store1.SaveMessage("user", "persisted", "", "test")
	store1.Close()

	// Re-open — should not error or lose data
	store2, err := NewStore(dbPath, 0)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	defer store2.Close()

	msgs, _ := store2.RecentMessages("test", 10)
	if len(msgs) != 1 {
		t.Errorf("got %d messages after re-open, want 1", len(msgs))
	}
}

// =====================================================================
// Summaries
// =====================================================================

func TestSaveSummary_RoundTrip(t *testing.T) {
	store := newCoreTestStore(t)

	id, err := store.SaveSummary("conv-1", "we talked about hiking", 1, 10, "chat")
	if err != nil {
		t.Fatalf("SaveSummary: %v", err)
	}
	if id == 0 {
		t.Fatal("SaveSummary returned id=0")
	}

	summary, endID, err := store.LatestSummary("conv-1", "chat")
	if err != nil {
		t.Fatalf("LatestSummary: %v", err)
	}
	if summary != "we talked about hiking" {
		t.Errorf("summary = %q, want %q", summary, "we talked about hiking")
	}
	if endID != 10 {
		t.Errorf("endID = %d, want 10", endID)
	}
}

func TestLatestSummary_NoSummary(t *testing.T) {
	store := newCoreTestStore(t)

	summary, endID, err := store.LatestSummary("nonexistent", "chat")
	if err != nil {
		t.Fatalf("LatestSummary error: %v", err)
	}
	if summary != "" {
		t.Errorf("summary = %q, want empty", summary)
	}
	if endID != 0 {
		t.Errorf("endID = %d, want 0", endID)
	}
}

func TestLatestSummary_StreamIsolation(t *testing.T) {
	store := newCoreTestStore(t)

	store.SaveSummary("conv-1", "chat summary", 1, 10, "chat")
	store.SaveSummary("conv-1", "agent summary", 1, 10, "agent")

	chatSum, _, _ := store.LatestSummary("conv-1", "chat")
	agentSum, _, _ := store.LatestSummary("conv-1", "agent")

	if chatSum != "chat summary" {
		t.Errorf("chat summary = %q, want %q", chatSum, "chat summary")
	}
	if agentSum != "agent summary" {
		t.Errorf("agent summary = %q, want %q", agentSum, "agent summary")
	}
}

func TestLatestSummary_ReturnsNewest(t *testing.T) {
	store := newCoreTestStore(t)

	store.SaveSummary("conv-1", "old summary", 1, 5, "chat")
	store.SaveSummary("conv-1", "new summary", 6, 15, "chat")

	summary, endID, _ := store.LatestSummary("conv-1", "chat")
	if summary != "new summary" {
		t.Errorf("summary = %q, want %q", summary, "new summary")
	}
	if endID != 15 {
		t.Errorf("endID = %d, want 15", endID)
	}
}

// =====================================================================
// PII Vault
// =====================================================================

func TestPIIVault_RoundTrip(t *testing.T) {
	store := newCoreTestStore(t)

	// Save a message first (for the foreign key)
	msgID, _ := store.SaveMessage("user", "call me at 555-1234", "", "conv-1")

	err := store.SavePIIVaultEntry(msgID, "[PHONE_1]", "555-1234", "phone")
	if err != nil {
		t.Fatalf("SavePIIVaultEntry: %v", err)
	}

	// Verify by direct query (no getter method exists, so we query raw).
	// Uses the DB() escape hatch rather than accessing the private db field.
	var token, original, entityType string
	err = store.DB().QueryRow(
		`SELECT token, original_value, entity_type FROM pii_vault WHERE message_id = ?`, msgID,
	).Scan(&token, &original, &entityType)
	if err != nil {
		t.Fatalf("query pii_vault: %v", err)
	}
	if token != "[PHONE_1]" {
		t.Errorf("token = %q, want %q", token, "[PHONE_1]")
	}
	if original != "555-1234" {
		t.Errorf("original = %q, want %q", original, "555-1234")
	}
	if entityType != "phone" {
		t.Errorf("entityType = %q, want %q", entityType, "phone")
	}
}

// =====================================================================
// Metrics
// =====================================================================

func TestSaveMetric_RoundTrip(t *testing.T) {
	store := newCoreTestStore(t)

	err := store.SaveMetric("gpt-4", 100, 50, 150, 0.003, 500, 0, false)
	if err != nil {
		t.Fatalf("SaveMetric: %v", err)
	}

	stats, err := store.GetStats()
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	if stats.TotalTokens != 150 {
		t.Errorf("TotalTokens = %d, want 150", stats.TotalTokens)
	}
}

func TestSaveMetric_WithMessageID(t *testing.T) {
	store := newCoreTestStore(t)

	msgID, _ := store.SaveMessage("user", "hello", "", "conv-1")
	err := store.SaveMetric("model-x", 10, 20, 30, 0.001, 200, msgID, false)
	if err != nil {
		t.Fatalf("SaveMetric with msgID: %v", err)
	}

	// Verify metric is linked to the message
	var linkedMsgID int64
	store.DB().QueryRow(`SELECT message_id FROM metrics WHERE model = 'model-x'`).Scan(&linkedMsgID)
	if linkedMsgID != msgID {
		t.Errorf("message_id = %d, want %d", linkedMsgID, msgID)
	}
}

func TestGetStats_Empty(t *testing.T) {
	store := newCoreTestStore(t)

	stats, err := store.GetStats()
	if err != nil {
		t.Fatalf("GetStats: %v", err)
	}
	if stats.TotalMessages != 0 {
		t.Errorf("TotalMessages = %d, want 0", stats.TotalMessages)
	}
	if stats.TotalTokens != 0 {
		t.Errorf("TotalTokens = %d, want 0", stats.TotalTokens)
	}
}

func TestGetUsageReport(t *testing.T) {
	store := newCoreTestStore(t)

	store.SaveMetric("model-a", 100, 50, 150, 0.003, 500, 0, false)
	store.SaveMetric("model-b", 200, 100, 300, 0.006, 300, 0, false)
	store.SaveMetric("model-a", 50, 25, 75, 0.001, 400, 0, false)

	report, err := store.GetUsageReport()
	if err != nil {
		t.Fatalf("GetUsageReport: %v", err)
	}

	// Should have 4 time periods
	if len(report.Periods) != 4 {
		t.Errorf("got %d periods, want 4", len(report.Periods))
	}

	// All-time should show all calls
	allTime := report.Periods[3]
	if allTime.Calls != 3 {
		t.Errorf("all-time calls = %d, want 3", allTime.Calls)
	}
	if allTime.Tokens != 525 {
		t.Errorf("all-time tokens = %d, want 525", allTime.Tokens)
	}

	// Per-model breakdown should have 2 models
	if len(report.ByModel) != 2 {
		t.Errorf("got %d models, want 2", len(report.ByModel))
	}
}

// =====================================================================
// Agent Turns
// =====================================================================

func TestSaveAgentTurn_RoundTrip(t *testing.T) {
	store := newCoreTestStore(t)

	msgID, _ := store.SaveMessage("user", "what's the weather?", "", "conv-1")

	// Save an agent turn sequence: think → tool call → tool result
	store.SaveAgentTurn(msgID, 0, "assistant", "think", `{"thought":"check weather"}`, "")
	store.SaveAgentTurn(msgID, 1, "assistant", "get_weather", `{"location":"NYC"}`, "")
	store.SaveAgentTurn(msgID, 2, "tool", "get_weather", "", "Sunny, 72F")

	actions, err := store.RecentAgentActions("conv-1", 1)
	if err != nil {
		t.Fatalf("RecentAgentActions: %v", err)
	}
	// The pairing logic combines assistant+tool rows, and skips unpaired think
	// think has no matching tool row, so it's emitted solo
	// get_weather assistant + tool = 1 paired action
	// think has no tool result partner, so it's also an action
	if len(actions) < 1 {
		t.Fatalf("got %d actions, want at least 1", len(actions))
	}

	// Find the weather action
	var found bool
	for _, a := range actions {
		if a.ToolName == "get_weather" {
			found = true
			if a.Result != "Sunny, 72F" {
				t.Errorf("Result = %q, want %q", a.Result, "Sunny, 72F")
			}
		}
	}
	if !found {
		t.Error("get_weather action not found in results")
	}
}

func TestSaveAgentTurn_ZeroMessageID(t *testing.T) {
	store := newCoreTestStore(t)

	// messageID=0 should be stored as NULL (not break the FK constraint)
	err := store.SaveAgentTurn(0, 0, "assistant", "think", "{}", "thinking...")
	if err != nil {
		t.Fatalf("SaveAgentTurn with 0 messageID: %v", err)
	}
}

// =====================================================================
// Pending Confirmations
// =====================================================================

func TestPendingConfirmation_Lifecycle(t *testing.T) {
	store := newCoreTestStore(t)

	payload, _ := json.Marshal(map[string]int{"expense_id": 42})

	id, err := store.CreatePendingConfirmation(12345, "delete_expense", payload, "Delete coffee expense?")
	if err != nil {
		t.Fatalf("CreatePendingConfirmation: %v", err)
	}
	if id == 0 {
		t.Fatal("CreatePendingConfirmation returned id=0")
	}

	// Retrieve it
	pc, err := store.GetPendingConfirmation(12345)
	if err != nil {
		t.Fatalf("GetPendingConfirmation: %v", err)
	}
	if pc == nil {
		t.Fatal("GetPendingConfirmation returned nil")
	}
	if pc.ActionType != "delete_expense" {
		t.Errorf("ActionType = %q, want %q", pc.ActionType, "delete_expense")
	}
	if pc.Description != "Delete coffee expense?" {
		t.Errorf("Description = %q, want %q", pc.Description, "Delete coffee expense?")
	}

	// Resolve it
	err = store.ResolvePendingConfirmation(pc.ID, "confirmed")
	if err != nil {
		t.Fatalf("ResolvePendingConfirmation: %v", err)
	}

	// Should no longer be retrievable (resolved)
	pc2, err := store.GetPendingConfirmation(12345)
	if err != nil {
		t.Fatalf("GetPendingConfirmation after resolve: %v", err)
	}
	if pc2 != nil {
		t.Error("resolved confirmation should not be returned")
	}
}

func TestGetPendingConfirmation_NotFound(t *testing.T) {
	store := newCoreTestStore(t)

	pc, err := store.GetPendingConfirmation(99999)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if pc != nil {
		t.Error("expected nil for nonexistent confirmation")
	}
}

// =====================================================================
// Searches + Classifier Log + Command Log
// =====================================================================

func TestSaveSearch(t *testing.T) {
	store := newCoreTestStore(t)

	err := store.SaveSearch(0, "web", "golang testing", "some results", 3)
	if err != nil {
		t.Fatalf("SaveSearch: %v", err)
	}

	// Verify via raw query using the DB() escape hatch
	var query, searchType string
	var resultCount int
	store.DB().QueryRow(`SELECT search_type, query, result_count FROM searches LIMIT 1`).
		Scan(&searchType, &query, &resultCount)
	if searchType != "web" {
		t.Errorf("search_type = %q, want %q", searchType, "web")
	}
	if query != "golang testing" {
		t.Errorf("query = %q, want %q", query, "golang testing")
	}
	if resultCount != 3 {
		t.Errorf("result_count = %d, want 3", resultCount)
	}
}

func TestSaveClassifierLog(t *testing.T) {
	store := newCoreTestStore(t)

	err := store.SaveClassifierLog("conv-1", "fact", "LOW_VALUE", "the sky is blue", "common knowledge", "")
	if err != nil {
		t.Fatalf("SaveClassifierLog: %v", err)
	}

	var verdict, content string
	store.DB().QueryRow(`SELECT verdict, content FROM classifier_log LIMIT 1`).Scan(&verdict, &content)
	if verdict != "LOW_VALUE" {
		t.Errorf("verdict = %q, want %q", verdict, "LOW_VALUE")
	}
	if content != "the sky is blue" {
		t.Errorf("content = %q, want %q", content, "the sky is blue")
	}
}

// =====================================================================
// Location History
// =====================================================================

func TestInsertLocation_RoundTrip(t *testing.T) {
	store := newCoreTestStore(t)

	err := store.InsertLocation(40.7128, -74.0060, "New York", "text", "conv-1")
	if err != nil {
		t.Fatalf("InsertLocation: %v", err)
	}

	loc := store.LatestLocation()
	if loc == nil {
		t.Fatal("LatestLocation returned nil")
	}
	if loc.Label != "New York" {
		t.Errorf("Label = %q, want %q", loc.Label, "New York")
	}
	if loc.Source != "text" {
		t.Errorf("Source = %q, want %q", loc.Source, "text")
	}
}

func TestLatestLocation_Empty(t *testing.T) {
	store := newCoreTestStore(t)

	loc := store.LatestLocation()
	if loc != nil {
		t.Errorf("expected nil for empty location_history, got %+v", loc)
	}
}
