package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os/exec"
	"regexp"
	"syscall"
	"time"

	"her/config"

	"github.com/spf13/cobra"
)

var devTunnelCmd = &cobra.Command{
	Use:   "dev-tunnel",
	Short: "Start a dev session with ephemeral tunnel and KV routing",
	Long: `Starts a development session with Cloudflare tunnel routing:

  1. Launches an ephemeral Cloudflare Tunnel (*.trycloudflare.com)
  2. Sets KV routing keys so the CF Worker sends traffic here
  3. Starts a heartbeat goroutine to keep the dev session alive
  4. Runs the bot in webhook mode
  5. On Ctrl+C: clears KV keys so prod resumes immediately

The VPS prod instance keeps running but receives no traffic while
dev-tunnel is active. When you stop, the CF Worker routes back to prod.

Requires cloudflare section in config.yaml (account_id, api_token, kv_namespace_id).`,
	RunE: runDevTunnel,
}

func init() {
	rootCmd.AddCommand(devTunnelCmd)
}

var trycloudflarePattern = regexp.MustCompile(`https://[a-zA-Z0-9-]+\.trycloudflare\.com`)

func runDevTunnel(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	if cfg.Cloudflare.AccountID == "" || cfg.Cloudflare.APIToken == "" || cfg.Cloudflare.KVNamespaceID == "" {
		return fmt.Errorf("cloudflare section in config.yaml is incomplete — need account_id, api_token, and kv_namespace_id for dev-tunnel mode")
	}

	if err := generateWranglerConfig(cfg); err != nil {
		log.Warn("could not generate wrangler.toml", "err", err)
	}

	cloudflaredBin, err := exec.LookPath("cloudflared")
	if err != nil {
		return fmt.Errorf("cloudflared not found — install with: brew install cloudflare/cloudflare/cloudflared")
	}

	webhookPort := cfg.Telegram.WebhookPort
	if webhookPort == 0 {
		webhookPort = 8443
	}

	log.Info("starting ephemeral tunnel...")
	tunnelURL, tunnelProcess, err := startEphemeralTunnel(cloudflaredBin, webhookPort)
	if err != nil {
		return fmt.Errorf("starting ephemeral tunnel: %w", err)
	}
	log.Info("tunnel ready", "url", tunnelURL)

	kv := &kvClient{
		accountID:   cfg.Cloudflare.AccountID,
		apiToken:    cfg.Cloudflare.APIToken,
		namespaceID: cfg.Cloudflare.KVNamespaceID,
		http:        &http.Client{Timeout: kvClientTimeout},
	}

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

	configTransform = func(c *config.Config) {
		c.Telegram.Mode = "webhook"
		log.Info("dev overrides applied", "mode", "webhook")
	}

	devCleanup = func() {
		log.Info("clearing KV routing keys...")
		heartbeatCancel()

		if err := kv.put("dev_session_ended", nowMillis()); err != nil {
			log.Warn("failed to write dev_session_ended", "err", err)
		}

		_ = kv.delete("dev_mode_active")
		_ = kv.delete("dev_instance_url")
		_ = kv.delete("dev_session_heartbeat")
		log.Info("KV cleared — traffic routed back to prod")

		killTunnel(tunnelProcess)
		log.Info("tunnel stopped")
	}

	return runBot(cmd, args)
}

func startEphemeralTunnel(cloudflaredBin string, port int) (string, *exec.Cmd, error) {
	proc := exec.Command(cloudflaredBin, "tunnel", "--url", fmt.Sprintf("http://localhost:%d", port))
	proc.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	stderr, err := proc.StderrPipe()
	if err != nil {
		return "", nil, fmt.Errorf("creating stderr pipe: %w", err)
	}
	proc.Stdout = io.Discard

	if err := proc.Start(); err != nil {
		return "", nil, fmt.Errorf("starting cloudflared: %w", err)
	}

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
		io.Copy(io.Discard, stderr)
	}()

	select {
	case url := <-urlCh:
		return url, proc, nil
	case <-time.After(30 * time.Second):
		killTunnel(proc)
		return "", nil, fmt.Errorf("timed out waiting for tunnel URL (30s)")
	}
}

func killTunnel(proc *exec.Cmd) {
	if proc != nil && proc.Process != nil {
		_ = syscall.Kill(-proc.Process.Pid, syscall.SIGKILL)
		_, _ = proc.Process.Wait()
	}
}
