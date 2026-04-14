package loader

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"
)

// proxyClient creates an http.Client that routes through the given proxy.
// This is how skills see the proxy — via HTTP_PROXY env var, which Go's
// net/http translates into this exact setup internally.
func proxyClient(proxyURL string) *http.Client {
	parsed, _ := url.Parse(proxyURL)
	return &http.Client{
		Transport: &http.Transport{
			Proxy: http.ProxyURL(parsed),
		},
		Timeout: 5 * time.Second,
	}
}

func TestSkillProxyStartsAndListens(t *testing.T) {
	proxy, err := NewSkillProxy()
	if err != nil {
		t.Fatalf("NewSkillProxy() error: %v", err)
	}
	defer proxy.Close()

	if proxy.Port() == 0 {
		t.Error("Port() should not be 0 after successful start")
	}

	want := fmt.Sprintf("http://127.0.0.1:%d", proxy.Port())
	if got := proxy.URL(); got != want {
		t.Errorf("URL() = %q, want %q", got, want)
	}
}

func TestSkillProxyBlocksSSRF(t *testing.T) {
	proxy, err := NewSkillProxy()
	if err != nil {
		t.Fatalf("NewSkillProxy() error: %v", err)
	}
	defer proxy.Close()

	client := proxyClient(proxy.URL())

	// Each of these is a private/reserved IP range that a malicious skill
	// might try to reach. The SSRF dialer blocks them all BEFORE the TCP
	// connection is established — the skill never touches these hosts.
	targets := []struct {
		name string
		url  string
	}{
		{"loopback", "http://127.0.0.1:8080/"},
		{"aws-metadata", "http://169.254.169.254/latest/meta-data/"},
		{"private-10", "http://10.0.0.1/"},
		{"private-192", "http://192.168.1.1/"},
		{"private-172", "http://172.16.0.1/"},
	}

	for _, tt := range targets {
		t.Run(tt.name, func(t *testing.T) {
			resp, err := client.Get(tt.url)
			if err != nil {
				// Connection-level error — proxy refused. This is fine.
				return
			}
			defer resp.Body.Close()

			// For HTTP (not HTTPS), goproxy catches the dial error and
			// returns a 502 Bad Gateway or 500 Internal Server Error
			// rather than a Go-level error. The skill still can't reach
			// the target — it just gets an error HTTP response instead.
			if resp.StatusCode == 200 {
				t.Errorf("expected SSRF block for %s, got HTTP 200", tt.url)
			}
		})
	}
}

func TestSkillProxyBlocksSSRFConnect(t *testing.T) {
	// HTTPS uses CONNECT tunnels, which go through a different code path
	// (ConnectDialWithReq instead of proxy.Tr). Make sure SSRF blocking
	// works there too.
	proxy, err := NewSkillProxy()
	if err != nil {
		t.Fatalf("NewSkillProxy() error: %v", err)
	}
	defer proxy.Close()

	client := proxyClient(proxy.URL())

	// HTTPS to loopback should be blocked by ConnectDialWithReq.
	resp, err := client.Get("https://127.0.0.1:443/")
	if err == nil {
		resp.Body.Close()
		t.Error("expected SSRF block for HTTPS to 127.0.0.1, but request succeeded")
	}
}

// ---------------------------------------------------------------------------
// Domain allowlist tests — real domains
// ---------------------------------------------------------------------------

func TestSkillProxyBlocksNonAllowedDomain(t *testing.T) {
	proxy, err := NewSkillProxy()
	if err != nil {
		t.Fatalf("NewSkillProxy() error: %v", err)
	}
	defer proxy.Close()

	// Set allowlist to ONLY example.com — everything else should be blocked.
	proxy.SetAllowedDomains([]string{"example.com"})
	defer proxy.ClearAllowedDomains()

	client := proxyClient(proxy.URL())

	// google.com is NOT in the allowlist — should get 403 Forbidden.
	resp, err := client.Get("http://google.com/")
	if err != nil {
		t.Fatalf("expected HTTP 403 from proxy, got error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 for non-allowed domain, got %d", resp.StatusCode)
	}
}

func TestSkillProxyBlocksNonAllowedDomainHTTPS(t *testing.T) {
	proxy, err := NewSkillProxy()
	if err != nil {
		t.Fatalf("NewSkillProxy() error: %v", err)
	}
	defer proxy.Close()

	// Only allow api.tavily.com — HTTPS to google.com should be rejected.
	proxy.SetAllowedDomains([]string{"api.tavily.com"})
	defer proxy.ClearAllowedDomains()

	client := proxyClient(proxy.URL())

	// HTTPS CONNECT to google.com should be rejected at the tunnel level.
	_, err = client.Get("https://google.com/")
	if err == nil {
		t.Error("expected CONNECT rejection for non-allowed HTTPS domain, but request succeeded")
	}
}

func TestSkillProxyAllowsListedDomain(t *testing.T) {
	proxy, err := NewSkillProxy()
	if err != nil {
		t.Fatalf("NewSkillProxy() error: %v", err)
	}
	defer proxy.Close()

	// example.com is in the allowlist — should go through.
	proxy.SetAllowedDomains([]string{"example.com"})
	defer proxy.ClearAllowedDomains()

	client := proxyClient(proxy.URL())

	// example.com is a real domain that returns 200. It's also one of the
	// most stable domains on the internet (owned by IANA for documentation).
	resp, err := client.Get("http://example.com/")
	if err != nil {
		t.Fatalf("expected request to allowed domain to succeed, got error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 for allowed domain, got %d", resp.StatusCode)
	}
}

func TestSkillProxyAllowsAllWhenNoAllowlist(t *testing.T) {
	proxy, err := NewSkillProxy()
	if err != nil {
		t.Fatalf("NewSkillProxy() error: %v", err)
	}
	defer proxy.Close()

	// No allowlist set (nil) — should allow everything.
	// This is the state when no untrusted skill is running.
	client := proxyClient(proxy.URL())

	resp, err := client.Get("http://example.com/")
	if err != nil {
		t.Fatalf("expected request to succeed with no allowlist, got error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200 with no allowlist, got %d", resp.StatusCode)
	}
}

func TestSkillProxyClearAllowlistRestoresAccess(t *testing.T) {
	proxy, err := NewSkillProxy()
	if err != nil {
		t.Fatalf("NewSkillProxy() error: %v", err)
	}
	defer proxy.Close()

	client := proxyClient(proxy.URL())

	// Set restrictive allowlist — google.com blocked.
	proxy.SetAllowedDomains([]string{"example.com"})
	resp, err := client.Get("http://google.com/")
	if err != nil {
		t.Fatalf("expected HTTP 403, got error: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 with allowlist active, got %d", resp.StatusCode)
	}

	// Clear allowlist — google.com should work again.
	proxy.ClearAllowedDomains()
	resp, err = client.Get("http://google.com/")
	if err != nil {
		t.Fatalf("expected request to succeed after clearing allowlist, got error: %v", err)
	}
	resp.Body.Close()
	// Any non-403 status means the domain filter isn't blocking anymore.
	if resp.StatusCode == http.StatusForbidden {
		t.Error("still getting 403 after clearing allowlist")
	}
}

func TestSkillProxyEmptyAllowlistBlocksEverything(t *testing.T) {
	proxy, err := NewSkillProxy()
	if err != nil {
		t.Fatalf("NewSkillProxy() error: %v", err)
	}
	defer proxy.Close()

	// Empty slice (not nil) means "skill declared no domains" — block all.
	proxy.SetAllowedDomains([]string{})

	client := proxyClient(proxy.URL())

	resp, err := client.Get("http://example.com/")
	if err != nil {
		t.Fatalf("expected HTTP 403, got error: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("expected 403 with empty allowlist, got %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// Trust tier integration tests
// ---------------------------------------------------------------------------

func TestSkillProxyByTrustTier(t *testing.T) {
	proxy, err := NewSkillProxy()
	if err != nil {
		t.Fatalf("NewSkillProxy() error: %v", err)
	}
	defer proxy.Close()

	// Simulate what the runner does: check AllowDirectNetwork, then
	// set/clear the allowlist accordingly. We test each trust tier.

	tests := []struct {
		name          string
		trust         TrustLevel
		domains       []string
		expectProxied bool   // should HTTP_PROXY be set?
		expectBlocked string // domain that should be blocked (empty = don't test)
		expectAllowed string // domain that should be allowed (empty = don't test)
	}{
		{
			name:          "1st-party: no proxy, direct access",
			trust:         TrustFirstParty,
			domains:       nil,
			expectProxied: false,
		},
		{
			name:          "2nd-party: no proxy, direct access",
			trust:         TrustSecondParty,
			domains:       []string{"api.tavily.com"},
			expectProxied: false,
		},
		{
			name:          "3rd-party: proxied, domain filtered",
			trust:         TrustThirdParty,
			domains:       []string{"example.com"},
			expectProxied: true,
			expectBlocked: "http://google.com/",
			expectAllowed: "http://example.com/",
		},
		{
			name:          "4th-party: proxied, domain filtered",
			trust:         TrustFourthParty,
			domains:       []string{"example.com"},
			expectProxied: true,
			expectBlocked: "http://google.com/",
			expectAllowed: "http://example.com/",
		},
		{
			name:          "4th-party: no domains declared, blocks everything",
			trust:         TrustFourthParty,
			domains:       []string{},
			expectProxied: true,
			expectBlocked: "http://example.com/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Check whether this tier would be proxied.
			shouldProxy := !tt.trust.AllowDirectNetwork()
			if shouldProxy != tt.expectProxied {
				t.Errorf("AllowDirectNetwork() for %s: got proxied=%v, want %v",
					tt.trust, shouldProxy, tt.expectProxied)
			}

			if !tt.expectProxied {
				// 1st/2nd party — no proxy involvement, nothing to test.
				return
			}

			// Simulate what the runner does for untrusted skills.
			proxy.SetAllowedDomains(tt.domains)
			defer proxy.ClearAllowedDomains()

			client := proxyClient(proxy.URL())

			// Test blocked domain.
			if tt.expectBlocked != "" {
				resp, err := client.Get(tt.expectBlocked)
				if err != nil {
					t.Fatalf("expected HTTP 403 for blocked domain, got error: %v", err)
				}
				resp.Body.Close()
				if resp.StatusCode != http.StatusForbidden {
					t.Errorf("blocked domain: expected 403, got %d", resp.StatusCode)
				}
			}

			// Test allowed domain.
			if tt.expectAllowed != "" {
				resp, err := client.Get(tt.expectAllowed)
				if err != nil {
					t.Fatalf("expected success for allowed domain, got error: %v", err)
				}
				resp.Body.Close()
				if resp.StatusCode == http.StatusForbidden {
					t.Errorf("allowed domain: got 403, expected pass-through")
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Lifecycle tests
// ---------------------------------------------------------------------------

func TestSkillProxyClose(t *testing.T) {
	proxy, err := NewSkillProxy()
	if err != nil {
		t.Fatalf("NewSkillProxy() error: %v", err)
	}

	// Close the proxy.
	if err := proxy.Close(); err != nil {
		t.Errorf("Close() error: %v", err)
	}

	// After close, new connections should be refused.
	client := proxyClient(proxy.URL())
	resp, err := client.Get("http://example.com")
	if err == nil {
		resp.Body.Close()
		t.Error("expected connection refused after Close(), but request succeeded")
	}
}
