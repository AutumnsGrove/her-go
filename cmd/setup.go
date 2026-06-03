package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/spf13/cobra"
	"golang.org/x/term"
	"her/config"
	"her/procmgr"
)

// isInteractive returns true if stdin is connected to a terminal.
// When running over an SSH pipe or inside a systemd unit, there's no TTY
// and interactive TUI forms (huh) will fail trying to open /dev/tty.
func isInteractive() bool {
	return term.IsTerminal(int(os.Stdin.Fd()))
}

// loadBotName loads config and returns just the bot's name.
// Used by service management commands (start, stop) that need the
// name for the service label but don't need full config.
func loadBotName() (string, error) {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return "", fmt.Errorf("loading config: %w", err)
	}
	return cfg.Identity.Her, nil
}

// loadServiceLabel loads config and returns the effective service label.
func loadServiceLabel() (string, error) {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return "", fmt.Errorf("loading config: %w", err)
	}
	return procmgr.EffectiveLabel(cfg.Update.ServiceLabel, cfg.Identity.Her), nil
}

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Build the binary, install dependencies, and configure the system service",
	Long: `Does everything needed to install her-go as a system service:

  1. Build the binary (go build)
  2. Install ML dependencies in background (piper voice models, ffmpeg check)
  3. Generate a service definition for the current platform (launchd on macOS, systemd on Linux)
  4. Create the logs directory
  5. Install and enable the service`,
	RunE: runSetup,
}

func init() {
	rootCmd.AddCommand(setupCmd)
}

// depResult holds the outcome of a background dependency install.
// Each goroutine fills one of these and we print them all at the end.
type depResult struct {
	Name    string
	OK      bool
	Message string
}

// installDeps runs all ML/tool dependency checks concurrently in the
// background. Returns a channel that will receive results as they complete.
// The caller should drain the channel after their own work is done.
//
// This uses sync.WaitGroup — Go's version of asyncio.gather(). You call
// wg.Add(1) for each goroutine you launch, wg.Done() when it finishes,
// and wg.Wait() blocks until all are done. We close the channel after
// Wait() so the caller knows there are no more results coming.
func installDeps() <-chan depResult {
	results := make(chan depResult, 5)
	var wg sync.WaitGroup

	// Check for uv (needed to install Python tools).
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := exec.LookPath("uv"); err != nil {
			results <- depResult{"uv", false, "not found — install from https://docs.astral.sh/uv/"}
			return
		}
		results <- depResult{"uv", true, "found"}
	}()

	// Check for ffmpeg (needed to convert Telegram .ogg voice memos).
	wg.Add(1)
	go func() {
		defer wg.Done()
		path, err := exec.LookPath("ffmpeg")
		if err != nil {
			results <- depResult{"ffmpeg", false, "not found — install with: brew install ffmpeg"}
			return
		}
		results <- depResult{"ffmpeg", true, path}
	}()

	// Parakeet (Apple MLX STT) — macOS only. On Linux, STT goes through
	// a remote Whisper endpoint instead, so there's nothing to install.
	if runtime.GOOS == "darwin" {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := exec.LookPath("uv"); err != nil {
				results <- depResult{"parakeet-mlx", false, "skipped (uv not available)"}
				return
			}
			out, err := exec.Command("uv", "tool", "install", "parakeet-mlx").CombinedOutput()
			msg := strings.TrimSpace(string(out))
			if err != nil {
				if strings.Contains(msg, "already installed") || strings.Contains(msg, "is already available") {
					results <- depResult{"parakeet-mlx", true, "already installed"}
					return
				}
				results <- depResult{"parakeet-mlx", false, msg}
				return
			}
			results <- depResult{"parakeet-mlx", true, "installed"}
		}()

		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := exec.LookPath("uv"); err != nil {
				results <- depResult{"parakeet-server", false, "skipped (uv not available)"}
				return
			}
			out, err := exec.Command("uv", "tool", "install",
				"git+https://github.com/yashhere/parakeet-mlx-fastapi.git").CombinedOutput()
			msg := strings.TrimSpace(string(out))
			if err != nil {
				if strings.Contains(msg, "already installed") || strings.Contains(msg, "is already available") {
					results <- depResult{"parakeet-server", true, "already installed"}
					return
				}
				results <- depResult{"parakeet-server", false, msg}
				return
			}
			results <- depResult{"parakeet-server", true, "installed"}
		}()
	}

	// Ollama (embedding server) — check it's available on any platform.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if _, err := exec.LookPath("ollama"); err != nil {
			results <- depResult{"ollama", false, "not found — install from https://ollama.com"}
			return
		}
		results <- depResult{"ollama", true, "found"}
	}()

	// Download Piper TTS voice model files.
	// These are ONNX model weights + a JSON config — we download them
	// once to scripts/piper-voices/. The tts_server.py loads them at startup.
	piperVoicesDir := filepath.Join("scripts", "piper-voices")
	piperFiles := []struct {
		name string
		url  string
		dest string
	}{
		{
			"piper-voice-model",
			"https://huggingface.co/rhasspy/piper-voices/resolve/main/en/en_GB/southern_english_female/low/en_GB-southern_english_female-low.onnx",
			filepath.Join(piperVoicesDir, "en_GB-southern_english_female-low.onnx"),
		},
		{
			"piper-voice-config",
			"https://huggingface.co/rhasspy/piper-voices/resolve/main/en/en_GB/southern_english_female/low/en_GB-southern_english_female-low.onnx.json",
			filepath.Join(piperVoicesDir, "en_GB-southern_english_female-low.onnx.json"),
		},
	}

	// Ensure the voices directory exists.
	_ = os.MkdirAll(piperVoicesDir, 0o755)

	for _, pf := range piperFiles {
		pf := pf // capture loop variable for the goroutine
		wg.Add(1)
		go func() {
			defer wg.Done()

			// Skip download if file already exists.
			if info, err := os.Stat(pf.dest); err == nil && info.Size() > 0 {
				results <- depResult{pf.name, true, fmt.Sprintf("already downloaded (%s)", pf.dest)}
				return
			}

			// Use curl to download — -L follows redirects (HuggingFace
			// redirects to CDN), --fail returns non-zero on HTTP errors.
			out, err := exec.Command("curl", "-L", "--fail", "-o", pf.dest, pf.url).CombinedOutput()
			msg := strings.TrimSpace(string(out))
			if err != nil {
				results <- depResult{pf.name, false, fmt.Sprintf("download failed: %s", msg)}
				return
			}
			results <- depResult{pf.name, true, fmt.Sprintf("downloaded to %s", pf.dest)}
		}()
	}

	// Close the channel once all goroutines finish.
	// This runs in its own goroutine so installDeps() returns immediately.
	go func() {
		wg.Wait()
		close(results)
	}()

	return results
}

func runSetup(cmd *cobra.Command, args []string) error {
	// Run the interactive wizard if we have a TTY. In headless environments
	// (SSH pipes, systemd, CI) the huh form can't open /dev/tty, so we skip
	// the wizard and rely on whatever config.yaml already has. This is like
	// checking sys.stdin.isatty() in Python before prompting for input.
	if isInteractive() {
		if err := runWizard(cfgFile); err != nil {
			if errors.Is(err, errWizardAborted) {
				return nil
			}
			return fmt.Errorf("setup wizard: %w", err)
		}
	} else {
		log.Info("no TTY detected — skipping interactive wizard, using existing config")
	}

	// Load config to get the bot's name for the service label.
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	// Gather machine info.
	hostname, _ := os.Hostname()
	currentUser, err := user.Current()
	if err != nil {
		return fmt.Errorf("could not determine current user: %w", err)
	}
	workDir, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("could not determine working directory: %w", err)
	}

	log.Infof("setting up %s on %s as %s", cfg.Identity.Her, hostname, currentUser.Username)
	log.Infof("working directory: %s", workDir)

	// Webhook mode: walk through prerequisites, deploy the CF Worker,
	// and register with Telegram. The preflight check ensures all deps
	// are installed and configured before we attempt the deploy.
	if cfg.Telegram.Mode == "webhook" {
		log.Info("webhook mode selected — checking prerequisites")
		ensureWebhookSecret(cfg)

		if err := webhookPreflight(cfg); err != nil {
			log.Error("webhook prerequisites not met", "err", err)
			log.Warn("fix the issue above and re-run `her setup`")
			log.Warn("the bot will fall back to poll mode until webhook is configured")
			cfg.Telegram.Mode = "poll"
			_ = saveConfig(cfg, cfgFile)
			return nil
		}

		// Prerequisites met — save any config changes from preflight
		// (e.g., auto-detected account ID, created KV namespace).
		if err := saveConfig(cfg, cfgFile); err != nil {
			return fmt.Errorf("saving config after preflight: %w", err)
		}

		log.Info("prerequisites satisfied — deploying CF Worker")
		if err := deployWebhook(cfg, cfgFile); err != nil {
			log.Error("webhook setup failed", "err", err)
			log.Warn("the bot will start in webhook mode but Telegram may not deliver updates")
			log.Warn("fix the issue above and re-run `her setup`, or switch to poll mode")
		}
	} else if cfg.Cloudflare.KVNamespaceID != "" {
		// Poll mode but CF is configured — just generate wrangler.toml
		// in case the user wants to deploy manually later.
		if err := generateWranglerConfig(cfg); err != nil {
			log.Warn("could not generate wrangler.toml", "err", err)
		}
	}

	// Kick off dependency installs in the background immediately.
	// These run concurrently while we build the binary and set up launchd.
	depResults := installDeps()

	// Step 1: Build the binary.
	log.Info("[1/4] building binary")
	binaryPath := filepath.Join(workDir, "her-go")
	buildCmd := exec.Command("go", "build", "-o", binaryPath, ".")
	buildCmd.Dir = workDir
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		return fmt.Errorf("go build failed: %w", err)
	}
	log.Infof("built: %s", binaryPath)

	// Step 2: Install the service via the platform's process manager.
	// On macOS this writes a plist to ~/Library/LaunchAgents and loads
	// it with launchctl. On Linux it writes a systemd unit file to
	// /etc/systemd/system and enables it.
	log.Infof("[2/4] installing %s service", runtime.GOOS)
	logsDir := filepath.Join(workDir, "logs")

	// Capture the user's current PATH so the service can find tools
	// like uv, piper, ffmpeg, ollama, etc. Without this, the supervisor
	// uses a bare-minimum PATH (/usr/bin:/bin) and sidecars fail.
	shellPath := os.Getenv("PATH")
	if shellPath == "" {
		shellPath = "/usr/local/bin:/usr/bin:/bin"
	}

	label := procmgr.EffectiveLabel(cfg.Update.ServiceLabel, cfg.Identity.Her)
	mgr, err := procmgr.New(label)
	if err != nil {
		return fmt.Errorf("creating process manager: %w", err)
	}

	svcCfg := procmgr.ServiceConfig{
		Label:      label,
		BinaryPath: binaryPath,
		WorkDir:    workDir,
		LogDir:     logsDir,
		User:       currentUser.Username,
		Path:       shellPath,
	}

	if err := mgr.Install(svcCfg); err != nil {
		log.Warn("failed to install service", "err", err)
	} else {
		log.Infof("service installed via %s", mgr.Name())
	}

	// Step 3: Wait for background dependency installs to finish.
	log.Info("[3/4] ML dependencies")
	var warnings []string
	for r := range depResults {
		if r.OK {
			log.Info("dep ok", "name", r.Name, "detail", r.Message)
		} else {
			warnings = append(warnings, fmt.Sprintf("%s: %s", r.Name, r.Message))
			log.Warn("dep missing", "name", r.Name, "detail", r.Message)
		}
	}

	// Step 4: Summary.
	log.Info("[4/4] setup complete",
		"binary", binaryPath,
		"supervisor", mgr.Name(),
		"logs", logsDir,
		"service", label,
	)

	for _, w := range warnings {
		log.Warn("missing dependency", "detail", w)
	}

	log.Info("commands: her start | her stop | her status | her logs")

	return nil
}
