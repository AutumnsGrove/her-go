package skillkit

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestDBClientQuery tests the Query method against a mock server.
//
// httptest.NewServer is Go's built-in test HTTP server — like Python's
// unittest.mock.patch but for HTTP. It starts a real server on localhost
// and gives you the URL to point your client at.
func TestDBClientQuery(t *testing.T) {
	// Mock server that returns canned responses.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Verify the request shape.
		if r.Method != "GET" {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/db/expenses" {
			t.Errorf("path = %s, want /db/expenses", r.URL.Path)
		}

		// Check query params.
		where := r.URL.Query().Get("where")
		if where != "amount > 50" {
			t.Errorf("where = %q, want %q", where, "amount > 50")
		}
		limit := r.URL.Query().Get("limit")
		if limit != "10" {
			t.Errorf("limit = %q, want %q", limit, "10")
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"rows": []map[string]any{
				{"id": 1, "amount": 89.99, "vendor": "Amazon"},
			},
			"count":  1,
			"limit":  10,
			"offset": 0,
		})
	}))
	defer server.Close()

	client := &DBClient{baseURL: server.URL, client: server.Client()}

	result, err := client.Query("expenses", QueryParams{
		Where: "amount > 50",
		Limit: 10,
	})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}

	if result.Count != 1 {
		t.Errorf("count = %d, want 1", result.Count)
	}
	if len(result.Rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(result.Rows))
	}
	if result.Rows[0]["vendor"] != "Amazon" {
		t.Errorf("vendor = %v, want Amazon", result.Rows[0]["vendor"])
	}
}

func TestDBClientInsert(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "POST" {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/db/mood_entries" {
			t.Errorf("path = %s, want /db/mood_entries", r.URL.Path)
		}

		// Verify the body was sent correctly.
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if body["rating"] != float64(4) {
			t.Errorf("rating = %v, want 4", body["rating"])
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		json.NewEncoder(w).Encode(map[string]any{
			"id":            42,
			"rows_affected": 1,
		})
	}))
	defer server.Close()

	client := &DBClient{baseURL: server.URL, client: server.Client()}

	id, err := client.Insert("mood_entries", map[string]any{
		"rating": 4,
		"note":   "good day",
	})
	if err != nil {
		t.Fatalf("Insert: %v", err)
	}
	if id != 42 {
		t.Errorf("id = %d, want 42", id)
	}
}

func TestDBClientUpdate(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "PUT" {
			t.Errorf("method = %s, want PUT", r.Method)
		}
		if r.URL.Path != "/db/expenses/7" {
			t.Errorf("path = %s, want /db/expenses/7", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"rows_affected": 1})
	}))
	defer server.Close()

	client := &DBClient{baseURL: server.URL, client: server.Client()}

	err := client.Update("expenses", "7", map[string]any{"note": "updated"})
	if err != nil {
		t.Fatalf("Update: %v", err)
	}
}

func TestDBClientDelete(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "DELETE" {
			t.Errorf("method = %s, want DELETE", r.Method)
		}
		if r.URL.Path != "/db/expenses/7" {
			t.Errorf("path = %s, want /db/expenses/7", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"rows_affected": 1})
	}))
	defer server.Close()

	client := &DBClient{baseURL: server.URL, client: server.Client()}

	err := client.Delete("expenses", "7")
	if err != nil {
		t.Fatalf("Delete: %v", err)
	}
}

func TestDBClientNoProxyURL(t *testing.T) {
	// Client with no baseURL should return a clear error.
	client := &DBClient{baseURL: "", client: http.DefaultClient}

	_, err := client.Query("expenses")
	if err == nil {
		t.Fatal("expected error when DB_PROXY_URL is not set")
	}

	_, err = client.Insert("expenses", map[string]any{"amount": 1})
	if err == nil {
		t.Fatal("expected error when DB_PROXY_URL is not set")
	}
}

func TestDBClientErrorResponse(t *testing.T) {
	// Server returns 403 with an error message.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		json.NewEncoder(w).Encode(map[string]string{
			"error": "access denied for table \"secrets\"",
		})
	}))
	defer server.Close()

	client := &DBClient{baseURL: server.URL, client: server.Client()}

	_, err := client.Query("secrets")
	if err == nil {
		t.Fatal("expected error for 403 response")
	}
	// Error should contain the proxy's message.
	if err.Error() == "" {
		t.Error("expected non-empty error message")
	}
}
