package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"text/template"
	"time"

	"her/config"

	"github.com/spf13/cobra"
)

var devCmd = &cobra.Command{
	Use:   "dev",
	Short: "Start a dev session with ephemeral tunnel and KV routing",
	Long: `Starts a development session on the MacBook:

  1. Launches an ephemeral Cloudflare Tunnel (*.trycloudflare.com)
  2. Sets KV routing keys so the CF Worker sends traffic here
  3. Starts a heartbeat goroutine to keep the dev session alive
  4. Runs the bot in webhook mode with a separate dev database
  5. On Ctrl+C: clears KV keys so prod resumes immediately

The Mac Mini prod instance keeps running but receives no traffic while
dev mode is active. When you stop, the CF Worker routes back to prod
within seconds.

Requires cloudflare section in config.yaml (account_id, api_token, kv_namespace_id).`,
	RunE: runDev,
}

func init() {
	rootCmd.AddCommand(devCmd)
}

// trycloudflarePattern matches the assigned tunnel URL from cloudflared output.
// cloudflared prints: "... https://random-words.trycloudflare.com ..."
var trycloudflarePattern = regexp.MustCompile(`https://[a-zA-Z0-9-]+\.trycloudflare\.com`)

// runDev orchestrates the dev session lifecycle.
func runDev(cmd *cobra.Command, args []string) error {
	// Load config to get Cloudflare credentials.
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Validate Cloudflare API credentials.
	if cfg.Cloudflare.AccountID == "" || cfg.Cloudflare.APIToken == "" || cfg.Cloudflare.KVNamespaceID == "" {
		return fmt.Errorf("cloudflare section in config.yaml is incomplete — need account_id, api_token, and kv_namespace_id for dev mode KV routing")
	}

	// Generate worker/wrangler.toml from config.yaml so IDs stay out
	// of version control. Non-fatal — wrangler.toml is only needed for
	// `npx wrangler deploy`, not for the bot itself.
	if err := generateWranglerConfig(cfg); err != nil {
		log.Warn("could not generate wrangler.toml", "err", err)
	}

	// Check cloudflared is installed.
	cloudflaredBin, err := exec.LookPath("cloudflared")
	if err != nil {
		return fmt.Errorf("cloudflared not found — install with: brew install cloudflare/cloudflare/cloudflared")
	}

	// Determine webhook port.
	webhookPort := cfg.Telegram.WebhookPort
	if webhookPort == 0 {
		webhookPort = 8443
	}

	// Step 1: Start ephemeral tunnel.
	log.Info("starting ephemeral tunnel...")
	tunnelURL, tunnelProcess, err := startEphemeralTunnel(cloudflaredBin, webhookPort)
	if err != nil {
		return fmt.Errorf("starting ephemeral tunnel: %w", err)
	}
	log.Info("tunnel ready", "url", tunnelURL)

	// Build the KV client from config.
	kv := &kvClient{
		accountID:   cfg.Cloudflare.AccountID,
		apiToken:    cfg.Cloudflare.APIToken,
		namespaceID: cfg.Cloudflare.KVNamespaceID,
		http:        &http.Client{Timeout: kvClientTimeout},
	}

	// Step 2: Set KV routing keys.
	log.Info("setting KV routing keys...")
	if err := kv.put("dev_mode_active", "true"); err != nil {
		killTunnel(tunnelProcess)
		return fmt.Errorf("setting dev_mode_active: %w", err)
	}
	if err := kv.put("dev_instance_url", tunnelURL); err != nil {
		killTunnel(tunnelProcess)
		return fmt.Errorf("setting dev_instance_url: %w", err)
	}
	if err := kv.put("dev_session_heartbeat", nowMillis()); err != nil {
		killTunnel(tunnelProcess)
		return fmt.Errorf("setting dev_session_heartbeat: %w", err)
	}
	log.Info("KV routing active — traffic redirected to dev")

	// Step 3: Start heartbeat goroutine (refreshes every 2 minutes).
	// The CF Worker considers the dev session stale after 5 minutes
	// without a heartbeat — this keeps it alive.
	heartbeatCtx, heartbeatCancel := context.WithCancel(context.Background())
	go func() {
		ticker := time.NewTicker(2 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ticker.C:
				if err := kv.put("dev_session_heartbeat", nowMillis()); err != nil {
					log.Warn("heartbeat failed", "err", err)
				}
			case <-heartbeatCtx.Done():
				return
			}
		}
	}()

	// Step 4: Set config transform so runBot uses webhook mode.
	// No separate dev DB — both machines use their own local her.db,
	// kept in sync via D1. The Pull on startup hydrates this machine's
	// her.db with any data the other machine created.
	configTransform = func(c *config.Config) {
		c.Telegram.Mode = "webhook"
		log.Info("dev overrides applied", "mode", "webhook")
	}

	// Step 5: Set cleanup hook — clears KV keys on shutdown so traffic
	// routes back to prod immediately.
	devCleanup = func() {
		log.Info("clearing KV routing keys...")
		heartbeatCancel()

		// Signal the prod instance that dev ended — it polls for this
		// key and triggers a D1 Pull to sync any data we created.
		if err := kv.put("dev_session_ended", nowMillis()); err != nil {
			log.Warn("failed to write dev_session_ended", "err", err)
		}

		// Best-effort cleanup — if these fail, the 5-minute heartbeat
		// timeout in the CF Worker handles it automatically.
		_ = kv.delete("dev_mode_active")
		_ = kv.delete("dev_instance_url")
		_ = kv.delete("dev_session_heartbeat")
		log.Info("KV cleared — traffic routed back to prod")

		// Kill the ephemeral tunnel process.
		killTunnel(tunnelProcess)
		log.Info("tunnel stopped")
	}

	// Step 6: Run the bot. This calls into the same runBot that `her run`
	// uses — the configTransform hook makes it use webhook mode + dev DB,
	// and devCleanup fires on shutdown.
	return runBot(cmd, args)
}

// startEphemeralTunnel launches `cloudflared tunnel --url` and captures the
// assigned *.trycloudflare.com URL from its output. This is a "quick tunnel"
// — no tunnel create/login needed, Cloudflare assigns a random URL.
//
// The function blocks until the URL is captured (up to 30 seconds).
func startEphemeralTunnel(cloudflaredBin string, port int) (string, *exec.Cmd, error) {
	proc := exec.Command(cloudflaredBin, "tunnel", "--url", fmt.Sprintf("http://localhost:%d", port))
	proc.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	// cloudflared prints the tunnel URL to stderr (not stdout).
	stderr, err := proc.StderrPipe()
	if err != nil {
		return "", nil, fmt.Errorf("creating stderr pipe: %w", err)
	}
	// Discard stdout — cloudflared writes nothing useful there.
	proc.Stdout = io.Discard

	if err := proc.Start(); err != nil {
		return "", nil, fmt.Errorf("starting cloudflared: %w", err)
	}

	// Scan stderr line by line for the trycloudflare.com URL.
	// cloudflared prints a decorative box around it:
	//   INF +---...---+
	//   INF |  Your quick Tunnel has been created! Visit it at: ...  |
	//   INF |  https://random-words.trycloudflare.com                |
	//   INF +---...---+
	urlCh := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(stderr)
		for scanner.Scan() {
			line := scanner.Text()
			if match := trycloudflarePattern.FindString(line); match != "" {
				urlCh <- match
				break
			}
		}
		// Keep draining stderr so cloudflared doesn't block on a full pipe.
		io.Copy(io.Discard, stderr)
	}()

	// Wait up to 30 seconds for the URL.
	select {
	case url := <-urlCh:
		return url, proc, nil
	case <-time.After(30 * time.Second):
		killTunnel(proc)
		return "", nil, fmt.Errorf("timed out waiting for tunnel URL (30s)")
	}
}

// killTunnel sends SIGKILL to the cloudflared process group.
func killTunnel(proc *exec.Cmd) {
	if proc != nil && proc.Process != nil {
		_ = syscall.Kill(-proc.Process.Pid, syscall.SIGKILL)
		_, _ = proc.Process.Wait()
	}
}

// --- Cloudflare Workers KV client ---
// Minimal HTTP client for the KV REST API. Only supports put and delete —
// that's all dev mode needs. Reading is done by the CF Worker at the edge.

type kvClient struct {
	accountID   string
	apiToken    string
	namespaceID string
	http        *http.Client
}

// kvBaseURL returns the API base for KV operations.
func (c *kvClient) kvBaseURL() string {
	return fmt.Sprintf("https://api.cloudflare.com/client/v4/accounts/%s/storage/kv/namespaces/%s", c.accountID, c.namespaceID)
}

// get reads a value from KV. Returns "" if the key doesn't exist.
// Used by the sync poller to check for the dev_session_ended signal.
func (c *kvClient) get(key string) (string, error) {
	url := c.kvBaseURL() + "/values/" + key
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiToken)

	resp, err := c.http.Do(req)
	if err != nil {
		return "", fmt.Errorf("kv get %s: %w", key, err)
	}
	defer resp.Body.Close()

	// 404 means the key doesn't exist — not an error, just empty.
	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("kv get %s: HTTP %d: %s", key, resp.StatusCode, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("kv get %s: reading body: %w", key, err)
	}
	return string(body), nil
}

// put writes a key-value pair to KV.
func (c *kvClient) put(key, value string) error {
	url := c.kvBaseURL() + "/values/" + key
	req, err := http.NewRequest("PUT", url, strings.NewReader(value))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiToken)
	req.Header.Set("Content-Type", "text/plain")

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("kv put %s: %w", key, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("kv put %s: HTTP %d: %s", key, resp.StatusCode, string(body))
	}
	return nil
}

// delete removes a key from KV.
func (c *kvClient) delete(key string) error {
	url := c.kvBaseURL() + "/values/" + key
	req, err := http.NewRequest("DELETE", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiToken)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("kv delete %s: %w", key, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("kv delete %s: HTTP %d: %s", key, resp.StatusCode, string(body))
	}
	return nil
}

// nowMillis returns the current time as a Unix millisecond string.
// The CF Worker parses this with parseInt() to check heartbeat freshness.
func nowMillis() string {
	return strconv.FormatInt(time.Now().UnixMilli(), 10)
}

// ---------------------------------------------------------------------------
// Wrangler config generator
// ---------------------------------------------------------------------------

// wranglerData holds the values injected into wrangler.toml.tmpl.
// All fields come from config.yaml — one source of truth.
type wranglerData struct {
	KVNamespaceID string
	D1DatabaseID  string
	ProdURL       string
}

// generateWranglerConfig reads worker/wrangler.toml.tmpl, fills in values
// from config.yaml, and writes worker/wrangler.toml. This keeps IDs out of
// version control — anyone can clone the repo, fill in config.yaml, and
// the wrangler config is derived automatically.
func generateWranglerConfig(cfg *config.Config) error {
	tmplPath := filepath.Join("worker", "wrangler.toml.tmpl")
	outPath := filepath.Join("worker", "wrangler.toml")

	tmplBytes, err := os.ReadFile(tmplPath)
	if err != nil {
		return fmt.Errorf("reading wrangler template: %w", err)
	}

	tmpl, err := template.New("wrangler").Parse(string(tmplBytes))
	if err != nil {
		return fmt.Errorf("parsing wrangler template: %w", err)
	}

	// Derive PROD_URL from tunnel domain. If no tunnel is configured,
	// leave it empty — the user hasn't set up production routing yet.
	prodURL := ""
	if cfg.Tunnel.Domain != "" {
		prodURL = "https://" + cfg.Tunnel.Domain
	}

	data := wranglerData{
		KVNamespaceID: cfg.Cloudflare.KVNamespaceID,
		D1DatabaseID:  cfg.Cloudflare.D1DatabaseID,
		ProdURL:       prodURL,
	}

	f, err := os.Create(outPath)
	if err != nil {
		return fmt.Errorf("creating wrangler.toml: %w", err)
	}
	defer f.Close()

	if err := tmpl.Execute(f, data); err != nil {
		return fmt.Errorf("writing wrangler.toml: %w", err)
	}

	log.Info("generated worker/wrangler.toml from config.yaml")
	return nil
}
