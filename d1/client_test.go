package d1

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// newTestClient creates a Client pointed at a local httptest.Server.
// The handler receives the raw request body and can return a canned response.
func newTestClient(t *testing.T, handler http.HandlerFunc) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)

	c := NewClient("test-account", "test-db", "test-token")
	c.baseURL = srv.URL // point at our test server instead of Cloudflare
	return c, srv
}

// TestNewClient_EmptyDatabaseID confirms that NewClient returns nil
// when the database ID is empty (sync disabled in config).
func TestNewClient_EmptyDatabaseID(t *testing.T) {
	c := NewClient("acct", "", "tok")
	if c != nil {
		t.Error("expected nil client when databaseID is empty")
	}
}

// TestQuery_SingleRow exercises the happy path: one SELECT returning one row.
func TestQuery_SingleRow(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		// Verify auth header.
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("auth header = %q, want Bearer test-token", got)
		}

		// Verify request body is a single statement (not batch).
		body, _ := io.ReadAll(r.Body)
		var req Statement
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("bad request body: %v", err)
		}
		if req.SQL != "SELECT * FROM memories WHERE id > ?" {
			t.Errorf("sql = %q, unexpected", req.SQL)
		}

		// Return a canned D1 response.
		json.NewEncoder(w).Encode(cfResponse{
			Success: true,
			Result: []QueryResult{
				{
					Success: true,
					Results: []Row{
						{"id": float64(43), "memory": "cats are great"},
					},
					Meta: Meta{RowsRead: 1},
				},
			},
		})
	})

	result, err := client.Query("SELECT * FROM memories WHERE id > ?", 42)
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(result.Results) != 1 {
		t.Fatalf("got %d rows, want 1", len(result.Results))
	}
	if result.Results[0]["memory"] != "cats are great" {
		t.Errorf("memory = %v, want 'cats are great'", result.Results[0]["memory"])
	}
}

// TestBatch_MultipleStatements exercises the batch path.
func TestBatch_MultipleStatements(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		// Verify it's a batch request (has "batch" key).
		var raw map[string]json.RawMessage
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatalf("bad request body: %v", err)
		}
		if _, ok := raw["batch"]; !ok {
			t.Error("expected batch key in request body")
		}

		// Return two results (one per statement).
		json.NewEncoder(w).Encode(cfResponse{
			Success: true,
			Result: []QueryResult{
				{Success: true, Meta: Meta{RowsWritten: 1}},
				{Success: true, Meta: Meta{RowsWritten: 1}},
			},
		})
	})

	results, err := client.Batch([]Statement{
		{SQL: "INSERT INTO memories (memory) VALUES (?)", Params: []any{"fact 1"}},
		{SQL: "INSERT INTO memories (memory) VALUES (?)", Params: []any{"fact 2"}},
	})
	if err != nil {
		t.Fatalf("Batch: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("got %d results, want 2", len(results))
	}
}

// TestBatch_Empty verifies that an empty batch returns nil without HTTP.
func TestBatch_Empty(t *testing.T) {
	// No test server needed — empty batch short-circuits.
	c := NewClient("acct", "db", "tok")
	results, err := c.Batch(nil)
	if err != nil {
		t.Fatalf("empty batch should not error: %v", err)
	}
	if results != nil {
		t.Errorf("expected nil results for empty batch, got %v", results)
	}
}

// TestQuery_APIError verifies error handling for non-200 responses.
func TestQuery_APIError(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(cfResponse{
			Success: false,
			Errors:  []cfError{{Code: 10000, Message: "Authentication error"}},
		})
	})

	_, err := client.Query("SELECT 1")
	if err == nil {
		t.Fatal("expected error for 403 response")
	}
	if got := err.Error(); got == "" {
		t.Error("error message should not be empty")
	}
}

// TestQuery_D1Failure verifies handling of 200 responses where success=false.
func TestQuery_D1Failure(t *testing.T) {
	client, _ := newTestClient(t, func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(cfResponse{
			Success: false,
			Errors:  []cfError{{Code: 7500, Message: "SQLITE_ERROR: no such table: bogus"}},
		})
	})

	_, err := client.Query("SELECT * FROM bogus")
	if err == nil {
		t.Fatal("expected error when D1 success=false")
	}
}
