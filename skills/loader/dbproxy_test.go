package loader

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	_ "github.com/mattn/go-sqlite3"
)

// newTestDB creates a temporary SQLite database with seed data for testing.
// Returns the DB path. The file is cleaned up when the test finishes.
//
// This is a common Go testing pattern: create disposable resources in a
// helper, use t.Cleanup to auto-remove them. Like Python's tmpdir fixture.
func newTestDB(t *testing.T) string {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := sql.Open("sqlite3", dbPath+"?_journal_mode=WAL")
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}

	// Create test tables with some seed data.
	_, err = db.Exec(`
		CREATE TABLE expenses (
			id       INTEGER PRIMARY KEY AUTOINCREMENT,
			amount   REAL NOT NULL,
			vendor   TEXT NOT NULL,
			category TEXT NOT NULL,
			date     TEXT NOT NULL
		);
		INSERT INTO expenses (amount, vendor, category, date) VALUES
			(42.50, 'Trader Joes', 'groceries', '2026-03-15'),
			(15.00, 'Netflix', 'subscriptions', '2026-03-01'),
			(89.99, 'Amazon', 'shopping', '2026-03-10');

		CREATE TABLE mood_entries (
			id        INTEGER PRIMARY KEY AUTOINCREMENT,
			rating    INTEGER NOT NULL,
			note      TEXT,
			source    TEXT DEFAULT 'manual',
			timestamp DATETIME DEFAULT CURRENT_TIMESTAMP
		);
		INSERT INTO mood_entries (rating, note) VALUES
			(4, 'good day'),
			(3, 'meh');

		CREATE TABLE secrets (
			id    INTEGER PRIMARY KEY,
			token TEXT NOT NULL
		);
		INSERT INTO secrets (token) VALUES ('super-secret-api-key');
	`)
	if err != nil {
		t.Fatalf("seed test db: %v", err)
	}
	db.Close()

	return dbPath
}

// newTestProxy creates a DBProxy backed by a temporary test database.
func newTestProxy(t *testing.T) (*DBProxy, string) {
	t.Helper()
	dbPath := newTestDB(t)
	proxy, err := NewDBProxy(dbPath)
	if err != nil {
		t.Fatalf("NewDBProxy: %v", err)
	}
	t.Cleanup(func() { proxy.Close() })
	return proxy, dbPath
}

// --- Phase 1 tests (lifecycle, permissions) ---

func TestDBProxyLifecycle(t *testing.T) {
	proxy, _ := newTestProxy(t)

	if proxy.Port() == 0 {
		t.Fatal("expected non-zero port")
	}

	expectedURL := fmt.Sprintf("http://127.0.0.1:%d", proxy.Port())
	if proxy.URL() != expectedURL {
		t.Fatalf("URL = %q, want %q", proxy.URL(), expectedURL)
	}
}

func TestDBProxyDeniesWithNoPermissions(t *testing.T) {
	proxy, _ := newTestProxy(t)

	// No permissions set = 403.
	resp, err := http.Get(proxy.URL() + "/db/expenses")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d (body: %s), want 403", resp.StatusCode, body)
	}
}

func TestDBProxyPermissionParsing(t *testing.T) {
	proxy, _ := newTestProxy(t)

	skill := &Skill{
		Name:       "expense_tracker",
		Dir:        t.TempDir(),
		TrustLevel: TrustSecondParty,
		Permissions: Permissions{
			DB: []string{"expenses:rw", "scheduled_tasks:r", "mood_entries"},
		},
	}
	proxy.SetPermissions(skill)

	proxy.mu.RLock()
	perms := proxy.permissions
	proxy.mu.RUnlock()

	for _, table := range []string{"expenses", "scheduled_tasks", "mood_entries"} {
		if !perms.readTables[table] {
			t.Errorf("expected %q in readTables", table)
		}
	}

	if !perms.writeTables["expenses"] {
		t.Error("expected expenses in writeTables")
	}
	if perms.writeTables["scheduled_tasks"] {
		t.Error("scheduled_tasks should not be writable")
	}
	if perms.writeTables["mood_entries"] {
		t.Error("mood_entries should not be writable (default :r)")
	}

	proxy.ClearPermissions()
}

func TestDBProxyThirdPartyDowngradedToReadOnly(t *testing.T) {
	proxy, _ := newTestProxy(t)

	skill := &Skill{
		Name:       "modified_skill",
		Dir:        t.TempDir(),
		TrustLevel: TrustThirdParty,
		Permissions: Permissions{
			DB: []string{"expenses:rw"},
		},
	}
	proxy.SetPermissions(skill)

	proxy.mu.RLock()
	perms := proxy.permissions
	proxy.mu.RUnlock()

	if !perms.readTables["expenses"] {
		t.Error("expected expenses in readTables")
	}
	if perms.writeTables["expenses"] {
		t.Error("3rd-party should not get write access")
	}

	proxy.ClearPermissions()
}

func TestDBProxyClearDeniesAll(t *testing.T) {
	proxy, _ := newTestProxy(t)

	skill := &Skill{
		Name:       "test_skill",
		Dir:        t.TempDir(),
		TrustLevel: TrustSecondParty,
		Permissions: Permissions{
			DB: []string{"expenses:rw"},
		},
	}
	proxy.SetPermissions(skill)
	proxy.ClearPermissions()

	resp, err := http.Get(proxy.URL() + "/db/expenses")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d (body: %s), want 403", resp.StatusCode, body)
	}
}

func TestParseDBPermission(t *testing.T) {
	tests := []struct {
		input     string
		wantTable string
		wantMode  string
	}{
		{"expenses:rw", "expenses", "rw"},
		{"scheduled_tasks:r", "scheduled_tasks", "r"},
		{"mood_entries", "mood_entries", "r"},
	}

	for _, tt := range tests {
		table, mode := parseDBPermission(tt.input)
		if table != tt.wantTable || mode != tt.wantMode {
			t.Errorf("parseDBPermission(%q) = (%q, %q), want (%q, %q)",
				tt.input, table, mode, tt.wantTable, tt.wantMode)
		}
	}
}

// --- Phase 2 tests (read endpoint, authorizer) ---

// queryResult is the JSON response from GET /db/{table}.
type queryResult struct {
	Rows   []map[string]any `json:"rows"`
	Count  int              `json:"count"`
	Limit  int              `json:"limit"`
	Offset int              `json:"offset"`
}

func TestReadAuthorizedTable(t *testing.T) {
	proxy, _ := newTestProxy(t)

	skill := &Skill{
		Name:       "reader",
		Dir:        t.TempDir(),
		TrustLevel: TrustSecondParty,
		Permissions: Permissions{
			DB: []string{"expenses:r"},
		},
	}
	proxy.SetPermissions(skill)
	defer proxy.ClearPermissions()

	resp, err := http.Get(proxy.URL() + "/db/expenses")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d (body: %s), want 200", resp.StatusCode, body)
	}

	var result queryResult
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// We seeded 3 expenses.
	if result.Count != 3 {
		t.Errorf("count = %d, want 3", result.Count)
	}
	if len(result.Rows) != 3 {
		t.Fatalf("rows = %d, want 3", len(result.Rows))
	}

	// Verify a row has the expected columns.
	row := result.Rows[0]
	for _, col := range []string{"id", "amount", "vendor", "category", "date"} {
		if _, ok := row[col]; !ok {
			t.Errorf("missing column %q in row", col)
		}
	}
}

func TestReadWithWhereClause(t *testing.T) {
	proxy, _ := newTestProxy(t)

	skill := &Skill{
		Name:       "reader",
		Dir:        t.TempDir(),
		TrustLevel: TrustSecondParty,
		Permissions: Permissions{
			DB: []string{"expenses:r"},
		},
	}
	proxy.SetPermissions(skill)
	defer proxy.ClearPermissions()

	// Filter to only groceries.
	resp, err := http.Get(proxy.URL() + "/db/expenses?where=category='groceries'")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d (body: %s), want 200", resp.StatusCode, body)
	}

	var result queryResult
	json.NewDecoder(resp.Body).Decode(&result)

	if result.Count != 1 {
		t.Errorf("count = %d, want 1 (only Trader Joes)", result.Count)
	}
}

func TestReadWithLimitOffset(t *testing.T) {
	proxy, _ := newTestProxy(t)

	skill := &Skill{
		Name:       "reader",
		Dir:        t.TempDir(),
		TrustLevel: TrustSecondParty,
		Permissions: Permissions{
			DB: []string{"expenses:r"},
		},
	}
	proxy.SetPermissions(skill)
	defer proxy.ClearPermissions()

	// Get only the first row.
	resp, err := http.Get(proxy.URL() + "/db/expenses?limit=1&offset=0")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	var result queryResult
	json.NewDecoder(resp.Body).Decode(&result)

	if result.Count != 1 {
		t.Errorf("count = %d, want 1", result.Count)
	}
	if result.Limit != 1 {
		t.Errorf("limit = %d, want 1", result.Limit)
	}

	// Get the second row.
	resp2, err := http.Get(proxy.URL() + "/db/expenses?limit=1&offset=1")
	if err != nil {
		t.Fatalf("GET offset: %v", err)
	}
	defer resp2.Body.Close()

	var result2 queryResult
	json.NewDecoder(resp2.Body).Decode(&result2)

	if result2.Count != 1 {
		t.Errorf("offset page count = %d, want 1", result2.Count)
	}
}

func TestReadUnauthorizedTableReturns403(t *testing.T) {
	proxy, _ := newTestProxy(t)

	// Skill has access to expenses but NOT secrets.
	skill := &Skill{
		Name:       "reader",
		Dir:        t.TempDir(),
		TrustLevel: TrustSecondParty,
		Permissions: Permissions{
			DB: []string{"expenses:r"},
		},
	}
	proxy.SetPermissions(skill)
	defer proxy.ClearPermissions()

	resp, err := http.Get(proxy.URL() + "/db/secrets")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d (body: %s), want 403", resp.StatusCode, body)
	}
}

func TestAuthorizerBlocksUnionInjection(t *testing.T) {
	proxy, _ := newTestProxy(t)

	// Skill has access to expenses but NOT secrets.
	skill := &Skill{
		Name:       "sneaky",
		Dir:        t.TempDir(),
		TrustLevel: TrustSecondParty,
		Permissions: Permissions{
			DB: []string{"expenses:r"},
		},
	}
	proxy.SetPermissions(skill)
	defer proxy.ClearPermissions()

	// Try a UNION injection to read the secrets table.
	// The authorizer should catch this — it fires SQLITE_READ for "secrets"
	// which is not in the allowlist.
	resp, err := http.Get(proxy.URL() + "/db/expenses?where=1=1 UNION SELECT 1,token,1,1,1 FROM secrets")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	// Should be blocked — either 403 (authorizer) or 500 (SQL error).
	// Either way, it should NOT be 200.
	if resp.StatusCode == http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("UNION injection succeeded! status 200, body: %s", body)
	}
}

func TestReadEmptyTableReturnsEmptyArray(t *testing.T) {
	proxy, _ := newTestProxy(t)

	skill := &Skill{
		Name:       "reader",
		Dir:        t.TempDir(),
		TrustLevel: TrustSecondParty,
		Permissions: Permissions{
			DB: []string{"expenses:r"},
		},
	}
	proxy.SetPermissions(skill)
	defer proxy.ClearPermissions()

	// WHERE clause that matches nothing.
	resp, err := http.Get(proxy.URL() + "/db/expenses?where=amount>99999")
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	var result queryResult
	json.NewDecoder(resp.Body).Decode(&result)

	if result.Count != 0 {
		t.Errorf("count = %d, want 0", result.Count)
	}
	if result.Rows == nil {
		t.Error("rows should be empty array, not null")
	}
}

// --- Phase 3 tests (WHERE clause validation) ---

func TestValidateWhereAllowed(t *testing.T) {
	// These WHERE clauses should all be accepted.
	// Table-driven tests are a Go convention — you define test cases as
	// a slice of structs and loop over them. Keeps tests DRY and makes
	// it easy to add new cases.
	allowed := []struct {
		name  string
		where string
	}{
		{"simple comparison", "amount > 50"},
		{"equality", "category = 'food'"},
		{"AND", "amount > 50 AND category = 'food'"},
		{"OR", "vendor = 'Amazon' OR vendor = 'Netflix'"},
		{"BETWEEN", "amount BETWEEN 10 AND 100"},
		{"IN literal list", "category IN ('groceries', 'shopping')"},
		{"LIKE", "vendor LIKE '%trader%'"},
		{"IS NULL", "note IS NULL"},
		{"IS NOT NULL", "note IS NOT NULL"},
		{"NOT", "NOT category = 'food'"},
		{"nested parens", "(amount > 50 AND category = 'food') OR vendor = 'Amazon'"},
		{"numeric comparison", "id >= 2"},
	}

	for _, tt := range allowed {
		t.Run(tt.name, func(t *testing.T) {
			if err := validateWhere(tt.where); err != nil {
				t.Errorf("validateWhere(%q) = %v, want nil", tt.where, err)
			}
		})
	}
}

func TestValidateWhereRejected(t *testing.T) {
	// These WHERE clauses should all be rejected by the parser.
	rejected := []struct {
		name  string
		where string
		want  string // substring expected in error message
	}{
		{
			"subquery EXISTS",
			"EXISTS (SELECT 1 FROM secrets)",
			"subqueries not allowed",
		},
		{
			"subquery IN SELECT",
			"id IN (SELECT id FROM secrets)",
			"subqueries not allowed",
		},
		{
			"load_extension",
			"load_extension('/tmp/evil.so')",
			"not allowed",
		},
		{
			"semicolon statement stacking",
			"1=1; DROP TABLE expenses",
			"semicolons not allowed",
		},
		{
			"fts3_tokenizer",
			"fts3_tokenizer('simple')",
			"not allowed",
		},
	}

	for _, tt := range rejected {
		t.Run(tt.name, func(t *testing.T) {
			err := validateWhere(tt.where)
			if err == nil {
				t.Errorf("validateWhere(%q) = nil, want error containing %q", tt.where, tt.want)
				return
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("validateWhere(%q) = %q, want error containing %q", tt.where, err, tt.want)
			}
		})
	}
}

func TestValidateWhereIntegration(t *testing.T) {
	// Test that the parser is actually called by the HTTP handler.
	proxy, _ := newTestProxy(t)

	skill := &Skill{
		Name:       "reader",
		Dir:        t.TempDir(),
		TrustLevel: TrustSecondParty,
		Permissions: Permissions{
			DB: []string{"expenses:r"},
		},
	}
	proxy.SetPermissions(skill)
	defer proxy.ClearPermissions()

	// A subquery should be rejected with 400 Bad Request.
	// Use a proper http.Request to avoid URL encoding issues with
	// spaces and parentheses in the WHERE clause.
	req, _ := http.NewRequest("GET", proxy.URL()+"/db/expenses", nil)
	q := req.URL.Query()
	q.Set("where", "id IN (SELECT id FROM secrets)")
	req.URL.RawQuery = q.Encode()

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d (body: %s), want 400", resp.StatusCode, body)
	}

	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result)
	if !strings.Contains(result["error"], "subqueries") {
		t.Errorf("error = %q, want to mention subqueries", result["error"])
	}
}

// --- Phase 4 tests (write endpoints, transactions) ---

// postJSON is a test helper that sends a POST with a JSON body.
func postJSON(url string, body map[string]any) (*http.Response, error) {
	data, _ := json.Marshal(body)
	return http.Post(url, "application/json", strings.NewReader(string(data)))
}

// putJSON sends a PUT with a JSON body.
func putJSON(url string, body map[string]any) (*http.Response, error) {
	data, _ := json.Marshal(body)
	req, _ := http.NewRequest("PUT", url, strings.NewReader(string(data)))
	req.Header.Set("Content-Type", "application/json")
	return http.DefaultClient.Do(req)
}

// doDelete sends a DELETE request.
func doDelete(url string) (*http.Response, error) {
	req, _ := http.NewRequest("DELETE", url, nil)
	return http.DefaultClient.Do(req)
}

func TestInsertAndReadBack(t *testing.T) {
	proxy, _ := newTestProxy(t)

	skill := &Skill{
		Name:       "writer",
		Dir:        t.TempDir(),
		TrustLevel: TrustSecondParty,
		Permissions: Permissions{
			DB: []string{"mood_entries:rw"},
		},
	}
	proxy.SetPermissions(skill)
	defer proxy.ClearPermissions()

	// Insert a new mood entry.
	resp, err := postJSON(proxy.URL()+"/db/mood_entries", map[string]any{
		"rating": 5,
		"note":   "amazing day",
		"source": "manual",
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("insert status = %d (body: %s), want 201", resp.StatusCode, body)
	}

	var insertResult map[string]any
	json.NewDecoder(resp.Body).Decode(&insertResult)
	if insertResult["id"] == nil || insertResult["id"].(float64) == 0 {
		t.Error("expected non-zero id from insert")
	}

	// Read it back. Use proper URL encoding — spaces in WHERE clauses
	// break raw URL strings (same issue we hit in Phase 3).
	req, _ := http.NewRequest("GET", proxy.URL()+"/db/mood_entries", nil)
	q := req.URL.Query()
	q.Set("where", "note='amazing day'")
	req.URL.RawQuery = q.Encode()
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET mood_entries: %v", err)
	}
	defer resp2.Body.Close()

	var readResult queryResult
	json.NewDecoder(resp2.Body).Decode(&readResult)

	if readResult.Count != 1 {
		t.Errorf("count = %d, want 1", readResult.Count)
	}
}

func TestUpdateRow(t *testing.T) {
	proxy, _ := newTestProxy(t)

	skill := &Skill{
		Name:       "writer",
		Dir:        t.TempDir(),
		TrustLevel: TrustSecondParty,
		Permissions: Permissions{
			DB: []string{"expenses:rw"},
		},
	}
	proxy.SetPermissions(skill)
	defer proxy.ClearPermissions()

	// Update the first expense's vendor.
	resp, err := putJSON(proxy.URL()+"/db/expenses/1", map[string]any{
		"vendor": "Trader Joe's",
	})
	if err != nil {
		t.Fatalf("PUT: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("update status = %d (body: %s), want 200", resp.StatusCode, body)
	}

	var result map[string]any
	json.NewDecoder(resp.Body).Decode(&result)
	if result["rows_affected"].(float64) != 1 {
		t.Errorf("rows_affected = %v, want 1", result["rows_affected"])
	}

	// Verify the update by reading it back by ID.
	req, _ := http.NewRequest("GET", proxy.URL()+"/db/expenses", nil)
	q := req.URL.Query()
	q.Set("where", "id=1")
	req.URL.RawQuery = q.Encode()
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET expenses: %v", err)
	}
	defer resp2.Body.Close()

	var readResult queryResult
	json.NewDecoder(resp2.Body).Decode(&readResult)

	if readResult.Count != 1 {
		t.Fatalf("updated row count = %d, want 1", readResult.Count)
	}
	vendor, _ := readResult.Rows[0]["vendor"].(string)
	if vendor != "Trader Joe's" {
		t.Errorf("vendor = %q, want %q", vendor, "Trader Joe's")
	}
}

func TestDeleteRow(t *testing.T) {
	proxy, _ := newTestProxy(t)

	skill := &Skill{
		Name:       "writer",
		Dir:        t.TempDir(),
		TrustLevel: TrustSecondParty,
		Permissions: Permissions{
			DB: []string{"expenses:rw"},
		},
	}
	proxy.SetPermissions(skill)
	defer proxy.ClearPermissions()

	// Delete expense ID 2 (Netflix).
	resp, err := doDelete(proxy.URL() + "/db/expenses/2")
	if err != nil {
		t.Fatalf("DELETE: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("delete status = %d (body: %s), want 200", resp.StatusCode, body)
	}

	// Verify: should now have 2 expenses instead of 3.
	resp2, err := http.Get(proxy.URL() + "/db/expenses")
	if err != nil {
		t.Fatalf("GET expenses: %v", err)
	}
	defer resp2.Body.Close()

	var result queryResult
	json.NewDecoder(resp2.Body).Decode(&result)
	if result.Count != 2 {
		t.Errorf("count after delete = %d, want 2", result.Count)
	}
}

func TestWriteDeniedForReadOnly(t *testing.T) {
	proxy, _ := newTestProxy(t)

	// Skill has read-only access to expenses.
	skill := &Skill{
		Name:       "reader",
		Dir:        t.TempDir(),
		TrustLevel: TrustSecondParty,
		Permissions: Permissions{
			DB: []string{"expenses:r"},
		},
	}
	proxy.SetPermissions(skill)
	defer proxy.ClearPermissions()

	// Try to insert — should be 403.
	resp, err := postJSON(proxy.URL()+"/db/expenses", map[string]any{
		"amount": 100, "vendor": "Evil Corp", "category": "misc", "date": "2026-01-01",
	})
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("write status = %d (body: %s), want 403", resp.StatusCode, body)
	}
}

func TestTransactionCommit(t *testing.T) {
	proxy, _ := newTestProxy(t)

	skill := &Skill{
		Name:       "tx_skill",
		Dir:        t.TempDir(),
		TrustLevel: TrustSecondParty,
		Permissions: Permissions{
			DB: []string{"expenses:rw"},
		},
	}
	proxy.SetPermissions(skill)
	defer proxy.ClearPermissions()

	// Begin a transaction.
	resp, err := http.Post(proxy.URL()+"/db/_tx/begin", "", nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("begin status = %d (body: %s), want 200", resp.StatusCode, body)
	}

	var beginResult map[string]string
	json.NewDecoder(resp.Body).Decode(&beginResult)
	txID := beginResult["tx_id"]
	if txID == "" {
		t.Fatal("expected tx_id in begin response")
	}

	// Insert within the transaction.
	data, _ := json.Marshal(map[string]any{
		"amount": 999, "vendor": "TxTest", "category": "test", "date": "2026-03-29",
	})
	req, _ := http.NewRequest("POST", proxy.URL()+"/db/expenses", strings.NewReader(string(data)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Transaction", txID)
	resp2, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST insert in tx: %v", err)
	}
	defer resp2.Body.Close()

	if resp2.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp2.Body)
		t.Fatalf("insert in tx status = %d (body: %s), want 201", resp2.StatusCode, body)
	}

	// Commit.
	resp3, err := http.Post(proxy.URL()+"/db/_tx/commit", "", nil)
	if err != nil {
		t.Fatalf("POST commit: %v", err)
	}
	defer resp3.Body.Close()

	if resp3.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp3.Body)
		t.Fatalf("commit status = %d (body: %s), want 200", resp3.StatusCode, body)
	}

	// Verify the row exists.
	resp4, err := http.Get(proxy.URL() + "/db/expenses?where=vendor='TxTest'")
	if err != nil {
		t.Fatalf("GET expenses after commit: %v", err)
	}
	defer resp4.Body.Close()

	var result queryResult
	json.NewDecoder(resp4.Body).Decode(&result)
	if result.Count != 1 {
		t.Errorf("committed row count = %d, want 1", result.Count)
	}
}

func TestTransactionRollback(t *testing.T) {
	proxy, _ := newTestProxy(t)

	skill := &Skill{
		Name:       "tx_skill",
		Dir:        t.TempDir(),
		TrustLevel: TrustSecondParty,
		Permissions: Permissions{
			DB: []string{"expenses:rw"},
		},
	}
	proxy.SetPermissions(skill)
	defer proxy.ClearPermissions()

	// Count expenses before.
	resp, err := http.Get(proxy.URL() + "/db/expenses")
	if err != nil {
		t.Fatalf("GET expenses before: %v", err)
	}
	defer resp.Body.Close()
	var before queryResult
	json.NewDecoder(resp.Body).Decode(&before)

	// Begin, insert, rollback.
	resp2, err := http.Post(proxy.URL()+"/db/_tx/begin", "", nil)
	if err != nil {
		t.Fatalf("POST begin: %v", err)
	}
	defer resp2.Body.Close()
	var beginResult map[string]string
	json.NewDecoder(resp2.Body).Decode(&beginResult)
	txID := beginResult["tx_id"]

	data, _ := json.Marshal(map[string]any{
		"amount": 888, "vendor": "RollbackTest", "category": "test", "date": "2026-03-29",
	})
	req, _ := http.NewRequest("POST", proxy.URL()+"/db/expenses", strings.NewReader(string(data)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Transaction", txID)
	resp3, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST insert in tx: %v", err)
	}
	defer resp3.Body.Close()

	// Rollback.
	resp4, err := http.Post(proxy.URL()+"/db/_tx/rollback", "", nil)
	if err != nil {
		t.Fatalf("POST rollback: %v", err)
	}
	defer resp4.Body.Close()

	// The inserted row should NOT exist.
	resp5, err := http.Get(proxy.URL() + "/db/expenses")
	if err != nil {
		t.Fatalf("GET expenses after rollback: %v", err)
	}
	defer resp5.Body.Close()
	var after queryResult
	json.NewDecoder(resp5.Body).Decode(&after)

	if after.Count != before.Count {
		t.Errorf("count after rollback = %d, want %d (same as before)", after.Count, before.Count)
	}
}

func TestThirdPartyCannotUseTransactions(t *testing.T) {
	proxy, _ := newTestProxy(t)

	skill := &Skill{
		Name:       "untrusted",
		Dir:        t.TempDir(),
		TrustLevel: TrustThirdParty,
		Permissions: Permissions{
			DB: []string{"expenses:r"},
		},
	}
	proxy.SetPermissions(skill)
	defer proxy.ClearPermissions()

	resp, err := http.Post(proxy.URL()+"/db/_tx/begin", "", nil)
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d (body: %s), want 403", resp.StatusCode, body)
	}
}
