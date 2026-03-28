package loader

import (
	"fmt"
	"net"
	"net/http"
	"sync"

	"code.dny.dev/ssrf"
	"github.com/elazarl/goproxy"
)

// SkillProxy is an HTTP/HTTPS forward proxy that sandboxes untrusted skill traffic.
//
// When a 3rd or 4th party skill runs, the runner sets HTTP_PROXY/HTTPS_PROXY
// env vars pointing here. All the skill's outbound HTTP traffic flows through
// this proxy, which blocks SSRF attacks (connections to private/loopback IPs).
//
// The proxy is transparent to the skill — it doesn't know it's being proxied.
// Go's net/http and Python's urllib/requests both respect the HTTP_PROXY env
// var automatically, so no code changes are needed in skill source.
//
// In Python terms, this is like running mitmproxy in the background and
// pointing your app at it via environment variables — except we're not doing
// MITM (no TLS interception), just domain-level filtering and SSRF prevention.
type SkillProxy struct {
	server   *goproxy.ProxyHttpServer
	listener net.Listener
	port     int

	// allowedDomains is the domain allowlist for the currently running skill.
	// Protected by mu because proxy handlers read it concurrently while the
	// runner writes it before each skill execution.
	//
	// sync.RWMutex is a reader-writer lock — multiple goroutines can read
	// simultaneously (RLock), but writes (Lock) are exclusive. This is the
	// Go equivalent of Python's threading.RLock, optimized for read-heavy
	// workloads (many proxy requests, rare allowlist changes).
	mu             sync.RWMutex
	allowedDomains map[string]bool // nil means "allow all" (no skill running)
}

// NewSkillProxy creates and starts the proxy on 127.0.0.1:0 (random port).
// The proxy is ready to accept connections when this function returns.
//
// The caller must call Close() during shutdown to release the port.
func NewSkillProxy() (*SkillProxy, error) {
	// --- SSRF-safe dialer ---
	//
	// This is the security core. ssrf.New() returns a "guardian" that blocks
	// connections to private IP ranges (127.0.0.1, 10.x, 192.168.x, etc.)
	// and cloud metadata endpoints (169.254.169.254).
	//
	// The magic is in net.Dialer.Control — it's a hook that runs AFTER DNS
	// resolution but BEFORE the TCP connection is established. So even if
	// an attacker makes evil.com resolve to 127.0.0.1 (DNS rebinding), we
	// catch it because we check the actual resolved IP, not the hostname.
	//
	// WithAnyPort() allows any destination port — we only care about IP
	// ranges, not which port the skill connects to.
	guardian := ssrf.New(ssrf.WithAnyPort())
	dialer := &net.Dialer{
		Control: guardian.Safe,
	}

	// --- Proxy server ---
	proxy := goproxy.NewProxyHttpServer()
	proxy.Verbose = false // we do our own logging

	// Wire the SSRF-safe dialer into the proxy's transport. This handles
	// plain HTTP requests (the proxy dials the target server on behalf of
	// the client).
	proxy.Tr = &http.Transport{
		DialContext: dialer.DialContext,
	}

	// CRITICAL: Also wire SSRF protection into HTTPS CONNECT tunnels.
	//
	// For HTTPS, the client sends a CONNECT request to the proxy, which
	// then dials the target and creates a raw TCP tunnel. Without this,
	// HTTPS requests would bypass our SSRF protection entirely because
	// they don't go through proxy.Tr.
	//
	// ConnectDialWithReq gives us access to the request context, which
	// DialContext needs for cancellation and timeouts.
	proxy.ConnectDialWithReq = func(req *http.Request, network, addr string) (net.Conn, error) {
		return dialer.DialContext(req.Context(), network, addr)
	}

	// Build the SkillProxy struct early so the handler closures can
	// reference it for domain allowlist checks.
	p := &SkillProxy{
		server: proxy,
	}

	// --- HTTP request handler ---
	// Checks every HTTP request against the active domain allowlist.
	// When allowedDomains is nil (no untrusted skill running), all traffic
	// passes through. When set, only declared domains are allowed.
	proxy.OnRequest().DoFunc(
		func(r *http.Request, ctx *goproxy.ProxyCtx) (*http.Request, *http.Response) {
			log.Debug("proxy request", "method", r.Method, "host", r.Host, "url", r.URL.String())

			if !p.isDomainAllowed(r.Host) {
				log.Warn("proxy blocked request", "host", r.Host, "url", r.URL.String())
				return r, goproxy.NewResponse(r, goproxy.ContentTypeText,
					http.StatusForbidden,
					fmt.Sprintf("domain %q not in skill's allowed domains", r.Host))
			}

			return r, nil
		},
	)

	// --- HTTPS CONNECT handler ---
	// For TLS connections, we only see the hostname (not the URL path or body)
	// because we don't do MITM. This is the right tradeoff: no CA cert
	// management, skills' TLS verification works normally, and we can still
	// filter by domain.
	proxy.OnRequest().HandleConnectFunc(
		func(host string, ctx *goproxy.ProxyCtx) (*goproxy.ConnectAction, string) {
			log.Debug("proxy CONNECT", "host", host)

			if !p.isDomainAllowed(host) {
				log.Warn("proxy blocked CONNECT", "host", host)
				return goproxy.RejectConnect, host
			}

			return goproxy.OkConnect, host
		},
	)

	// --- Listen on random port ---
	// 127.0.0.1:0 means "localhost, any available port". The OS assigns a
	// port, which we read back. This avoids conflicts with other services.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("proxy listen: %w", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port

	// Serve in a background goroutine. http.Serve blocks until the
	// listener is closed — at that point it returns an error, which
	// is expected during shutdown.
	go func() {
		if err := http.Serve(listener, proxy); err != nil {
			// "use of closed network connection" is expected on shutdown.
			log.Debug("proxy server stopped", "reason", err)
		}
	}()

	// Fill in the remaining fields now that we have the listener.
	p.listener = listener
	p.port = port

	log.Info("skill proxy started", "port", port, "addr", "127.0.0.1")
	return p, nil
}

// Port returns the TCP port the proxy is listening on.
func (p *SkillProxy) Port() int {
	return p.port
}

// URL returns the full proxy URL (e.g., "http://127.0.0.1:54321").
// This is what gets set as HTTP_PROXY in the skill's environment.
func (p *SkillProxy) URL() string {
	return fmt.Sprintf("http://127.0.0.1:%d", p.port)
}

// SetAllowedDomains sets the domain allowlist for the currently running skill.
// Only these domains will be reachable through the proxy — all other requests
// get a 403 (HTTP) or connection refused (HTTPS CONNECT).
//
// The runner calls this before executing an untrusted skill, passing in the
// skill's Permissions.Domains list. After the skill finishes, call
// ClearAllowedDomains to remove the restriction.
//
// An empty slice means "block everything" — the skill declared no domains.
// To allow everything, call ClearAllowedDomains instead.
func (p *SkillProxy) SetAllowedDomains(domains []string) {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.allowedDomains = make(map[string]bool, len(domains))
	for _, d := range domains {
		p.allowedDomains[d] = true
	}
	log.Debug("proxy allowlist set", "domains", domains)
}

// ClearAllowedDomains removes the domain restriction, allowing all traffic.
// Called after an untrusted skill finishes executing.
func (p *SkillProxy) ClearAllowedDomains() {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.allowedDomains = nil
	log.Debug("proxy allowlist cleared")
}

// isDomainAllowed checks whether a host (with optional port) is in the
// active allowlist. Returns true if no allowlist is set (nil = allow all).
//
// The host parameter comes in different forms:
//   - HTTP requests: "example.com" or "example.com:8080"
//   - CONNECT tunnels: "example.com:443"
//
// We strip the port before checking, since the allowlist is domain-only.
func (p *SkillProxy) isDomainAllowed(host string) bool {
	p.mu.RLock()
	defer p.mu.RUnlock()

	// nil means no restriction (no untrusted skill running, or 2nd-party).
	if p.allowedDomains == nil {
		return true
	}

	// Strip port if present. net.SplitHostPort fails if there's no port,
	// which is fine — we just use the original host.
	domain := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		domain = h
	}

	return p.allowedDomains[domain]
}

// Close shuts down the proxy listener. In-flight connections are terminated.
// Called during application shutdown from cmd/run.go.
func (p *SkillProxy) Close() error {
	log.Info("skill proxy stopping", "port", p.port)
	return p.listener.Close()
}
