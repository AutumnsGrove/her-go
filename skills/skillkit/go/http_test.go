package skillkit

import (
	"net/http"
	"testing"
	"time"
)

// TestHTTPClientTimeout verifies the client has a sensible timeout.
// Go's default http.Client has ZERO timeout — requests hang forever.
// We need to make sure ours doesn't.
func TestHTTPClientTimeout(t *testing.T) {
	client := HTTPClient()
	if client.Timeout != 30*time.Second {
		t.Errorf("Timeout = %v, want 30s", client.Timeout)
	}
}

// TestHTTPClientHasTransport verifies the client uses a custom transport
// (with proxy support) rather than the default.
func TestHTTPClientHasTransport(t *testing.T) {
	client := HTTPClient()

	transport, ok := client.Transport.(*http.Transport)
	if !ok {
		t.Fatal("Transport is not *http.Transport")
	}

	// Proxy function should be set (ProxyFromEnvironment).
	// We can't compare functions directly in Go, but we can verify
	// it's not nil — which means proxy support is wired up.
	if transport.Proxy == nil {
		t.Error("Proxy function is nil, expected ProxyFromEnvironment")
	}
}

// TestHTTPClientReturnsNewInstance verifies each call returns a fresh
// client. Skills shouldn't share clients — one skill's cancelled
// context shouldn't affect another.
func TestHTTPClientReturnsNewInstance(t *testing.T) {
	a := HTTPClient()
	b := HTTPClient()
	if a == b {
		t.Error("HTTPClient() returned the same pointer twice")
	}
}
