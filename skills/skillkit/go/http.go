package skillkit

import (
	"net/http"
	"time"
)

// HTTPClient returns an *http.Client that respects HTTP_PROXY and
// HTTPS_PROXY environment variables. Skills use this for all outbound
// HTTP requests.
//
// Why a wrapper instead of just using http.DefaultClient?
//
//  1. Proxy transparency — the skill trust model (see docs/skills-architecture.md)
//     routes untrusted skills through a network proxy. The harness sets
//     HTTP_PROXY before launching the skill binary. This client picks it
//     up automatically via http.ProxyFromEnvironment.
//
//  2. Timeouts — Go's http.DefaultClient has NO timeout by default (!).
//     A skill that hits a slow endpoint would hang forever. We set a
//     sensible 30-second timeout.
//
// In Python, requests.get() respects HTTP_PROXY and has a timeout param.
// This is the Go equivalent — same idea, just explicit.
//
// Usage:
//
//	client := skillkit.HTTPClient()
//	resp, err := client.Get("https://api.example.com/data")
func HTTPClient() *http.Client {
	return &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			// ProxyFromEnvironment reads HTTP_PROXY, HTTPS_PROXY, and
			// NO_PROXY env vars. If none are set, it connects directly.
			// This is what makes the proxy invisible to skill code.
			Proxy: http.ProxyFromEnvironment,
		},
	}
}
