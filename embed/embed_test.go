package embed

// Unit tests for Client.IsAvailable().
//
// IsAvailable() is the health check that the startup lifecycle uses to decide
// whether the embedding sidecar is reachable before launching web_search and
// recall_memories. It POSTs a tiny probe to {baseURL}/embeddings with a
// 2-second timeout and returns true if the status is 2xx.
//
// We test it with httptest.NewServer() — the same pattern used in
// agent/memory_agent_test.go. This gives us a real TCP listener on localhost
// with no external dependencies, and closes cleanly via defer.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestIsAvailable_Up verifies that a reachable server returning 200 causes
// IsAvailable to return true. This is the happy path: LM Studio is loaded and
// responding normally.
func TestIsAvailable_Up(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Any 2xx status is enough — we don't validate the response body.
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	// NewClient: baseURL, model, apiKey (empty = no auth header), dimension.
	c := NewClient(srv.URL, "test-model", "", 768)
	if !c.IsAvailable() {
		t.Error("IsAvailable() = false, want true when server returns 200")
	}
}

// TestIsAvailable_Down verifies that an unreachable server (connection refused)
// causes IsAvailable to return false. This is the "sidecar not started yet"
// scenario — the startup lifecycle should handle this by launching the sidecar.
func TestIsAvailable_Down(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	// Close the server before calling IsAvailable so any connection attempt
	// gets an immediate ECONNREFUSED — much faster than a timeout test.
	srv.Close()

	c := NewClient(srv.URL, "test-model", "", 768)
	if c.IsAvailable() {
		t.Error("IsAvailable() = true, want false when server is down")
	}
}

// TestIsAvailable_ServerError verifies that a reachable server returning a
// non-2xx status (500 Internal Server Error) causes IsAvailable to return false.
// This covers the case where LM Studio is running but crashed or is unresponsive
// at the application level.
func TestIsAvailable_ServerError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	c := NewClient(srv.URL, "test-model", "", 768)
	if c.IsAvailable() {
		t.Error("IsAvailable() = true, want false when server returns 500")
	}
}
