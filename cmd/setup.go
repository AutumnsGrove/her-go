package cmd

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"text/template"

	"github.com/spf13/cobra"
	"her/config"
)

// serviceLabel builds the launchd service identifier from the bot's
// configured name. e.g. "Mira" → "com.mira.her-go", "Luna" → "com.luna.her-go".
func serviceLabel(botName string) string {
	return "com." + strings.ToLower(botName) + ".her-go"
}

// domainTarget returns the launchd domain for the current user, e.g.
// "gui/501". Used with the modern launchctl subcommands (bootstrap,
// bootout, kickstart, print) which replaced the deprecated load/unload.
func domainTarget() string {
	return fmt.Sprintf("gui/%d", os.Getuid())
}

// serviceTarget returns the fully-qualified launchd service target for
// the current user, e.g. "gui/501/com.mira.her-go". Used with bootout,
// kickstart, and print.
func serviceTarget(label string) string {
	return fmt.Sprintf("gui/%d/%s", os.Getuid(), label)
}

// launchdBootstrap loads a service plist into the user's launchd domain
// using the modern bootstrap command. If the service is already loaded,
// it bootouts first and retries — handling the common "re-setup" case
// where the user runs setup again on a running service.
func launchdBootstrap(plistPath, label string) error {
	domain := domainTarget()
	out, err := exec.Command("launchctl", "bootstrap", domain, plistPath).CombinedOutput()
	if err == nil {
		return nil
	}

	outStr := strings.TrimSpace(string(out))

	// When a service is already loaded, bootstrap fails with either:
	//   - Exit 5 + "Bootstrap failed: 5: Input/output error" (macOS 13+)
	//   - Exit 37 + text containing "already loaded" (older macOS)
	// In both cases, bootout first then retry.
	if strings.Contains(outStr, "Bootstrap failed: 5:") || strings.Contains(outStr, "37:") ||
		strings.Contains(outStr, "already loaded") {
		log.Info("service already loaded, reloading")
		_ = exec.Command("launchctl", "bootout", serviceTarget(label)).Run()
		out2, err2 := exec.Command("launchctl", "bootstrap", domain, plistPath).CombinedOutput()
		if err2 != nil {
			return fmt.Errorf("launchctl bootstrap (retry): %s", strings.TrimSpace(string(out2)))
		}
		return nil
	}

	return fmt.Errorf("launchctl bootstrap: %s", outStr)
}

// launchdBootout unloads a service from the user's launchd domain using
// the modern bootout command.
func launchdBootout(label string) error {
	out, err := exec.Command("launchctl", "bootout", serviceTarget(label)).CombinedOutput()
	if err != nil {
		return fmt.Errorf("launchctl bootout: %s", strings.TrimSpace(string(out)))
	}
	return nil
}

// plistPath returns the full path to the plist in ~/Library/LaunchAgents.
func plistPath(botName string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not determine home directory: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", serviceLabel(botName)+".plist"), nil
}

// loadBotName loads config and returns just the bot's name.
// Used by service management commands (start, stop) that need the
// name for the launchd service label but don't need full config.
func loadBotName() (string, error) {
	cfg, err := config.Load(cfgFile)
	if err != nil {
		return "", fmt.Errorf("loading config: %w", err)
	}
	return cfg.Identity.Her, nil
}

// plistData holds the values injected into the plist template.
type plistData struct {
	Label      string
	BinaryPath string
	WorkDir    string
	StdoutPath string
	StderrPath string
	UserName   string
	Path       string // PATH inherited from the user's shell at setup time
}

// plistTemplate is the launchd property list, generated dynamically
// so it always matches the current machine's paths and user.
const plistTemplate = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
    <key>Label</key>
    <string>{{.Label}}</string>

    <key>ProgramArguments</key>
    <array>
        <string>{{.BinaryPath}}</string>
        <string>run</string>
    </array>

    <key>WorkingDirectory</key>
    <string>{{.WorkDir}}</string>

    <key>KeepAlive</key>
    <true/>

    <key>ThrottleInterval</key>
    <integer>3</integer>

    <key>StandardOutPath</key>
    <string>{{.StdoutPath}}</string>

    <key>StandardErrorPath</key>
    <string>{{.StderrPath}}</string>

    <key>EnvironmentVariables</key>
    <dict>
        <key>PATH</key>
        <string>{{.Path}}</string>
    </dict>

    <key>UserName</key>
    <string>{{.UserName}}</string>
</dict>
</plist>
`

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Build the binary, install dependencies, and configure the launchd service",
	Long: `Does everything needed to install her-go as a launchd service:

  1. Build the binary (go build)
  2. Install ML dependencies in background (parakeet-mlx, piper voice models, ffmpeg check)
  3. Generate a plist file for the current machine
  4. Create the logs directory
  5. Install the plist to ~/Library/LaunchAgents
  6. Load the service with launchctl`,
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

	// Install parakeet-mlx (STT inference library + CLI).
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
			// "already installed" is not an error — uv returns non-zero
			// but the tool is there. Check the output message.
			if strings.Contains(msg, "already installed") || strings.Contains(msg, "is already available") {
				results <- depResult{"parakeet-mlx", true, "already installed"}
				return
			}
			results <- depResult{"parakeet-mlx", false, msg}
			return
		}
		results <- depResult{"parakeet-mlx", true, "installed"}
	}()

	// Install parakeet-mlx-fastapi (HTTP server for STT).
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
	// Run the interactive wizard first — walks through config.yaml fields
	// and writes the file before we build or install anything. If the user
	// quits mid-wizard nothing is written and setup stops cleanly.
	if err := runWizard(cfgFile); err != nil {
		if errors.Is(err, errWizardAborted) {
			return nil
		}
		return fmt.Errorf("setup wizard: %w", err)
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
	log.Info("[1/6] building binary")
	binaryPath := filepath.Join(workDir, "her-go")
	buildCmd := exec.Command("go", "build", "-o", binaryPath, ".")
	buildCmd.Dir = workDir
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		return fmt.Errorf("go build failed: %w", err)
	}
	log.Infof("built: %s", binaryPath)

	// Step 2: Generate the plist.
	log.Info("[2/6] generating plist")
	logsDir := filepath.Join(workDir, "logs")
	// Capture the user's current PATH so the launchd service can find
	// tools like uv, parakeet-server, ffmpeg, etc. Without this, launchd
	// uses a bare-minimum PATH (/usr/bin:/bin) and sidecars fail to start.
	// We snapshot it at setup time — if the user installs new tools later,
	// they just re-run `her setup` to pick up the new paths.
	shellPath := os.Getenv("PATH")
	if shellPath == "" {
		shellPath = "/usr/local/bin:/usr/bin:/bin"
	}

	data := plistData{
		Label:      serviceLabel(cfg.Identity.Her),
		BinaryPath: binaryPath,
		WorkDir:    workDir,
		StdoutPath: filepath.Join(logsDir, "stdout.log"),
		StderrPath: filepath.Join(logsDir, "stderr.log"),
		UserName:   currentUser.Username,
		Path:       shellPath,
	}

	tmpl, err := template.New("plist").Parse(plistTemplate)
	if err != nil {
		return fmt.Errorf("failed to parse plist template: %w", err)
	}

	dest, err := plistPath(cfg.Identity.Her)
	if err != nil {
		return err
	}

	plistFile, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("failed to create plist file %s: %w", dest, err)
	}
	if err := tmpl.Execute(plistFile, data); err != nil {
		plistFile.Close()
		return fmt.Errorf("failed to write plist: %w", err)
	}
	plistFile.Close()
	log.Infof("wrote: %s", dest)

	// Step 3: Create logs directory.
	log.Info("[3/6] creating logs directory")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return fmt.Errorf("failed to create logs directory: %w", err)
	}
	log.Infof("dir: %s", logsDir)

	// Step 4: (plist already written to ~/Library/LaunchAgents in step 2)
	log.Info("[4/6] plist installed to ~/Library/LaunchAgents")

	// Step 5: Load the service using the modern launchctl bootstrap command.
	// The old "launchctl load" is deprecated and can silently fail (printing
	// errors to stderr while returning exit 0). bootstrap has proper error
	// codes and handles the "already loaded" case via automatic bootout+retry.
	log.Info("[5/6] loading service")
	label := serviceLabel(cfg.Identity.Her)
	if err := launchdBootstrap(dest, label); err != nil {
		log.Warn("failed to load service", "err", err)
	} else {
		log.Info("service loaded")
	}

	// Step 6: Wait for background dependency installs to finish.
	// The channel was created in installDeps() — ranging over it gives
	// us each result as it arrives, and stops when the channel closes
	// (which happens after all goroutines call wg.Done()).
	log.Info("[6/6] ML dependencies")
	var warnings []string
	for r := range depResults {
		if r.OK {
			log.Info("dep ok", "name", r.Name, "detail", r.Message)
		} else {
			warnings = append(warnings, fmt.Sprintf("%s: %s", r.Name, r.Message))
			log.Warn("dep missing", "name", r.Name, "detail", r.Message)
		}
	}

	// Summary.
	log.Info("setup complete",
		"binary", binaryPath,
		"plist", dest,
		"logs", logsDir,
		"service", serviceLabel(cfg.Identity.Her),
	)

	for _, w := range warnings {
		log.Warn("missing dependency", "detail", w)
	}

	log.Info("commands: her start | her stop | her status | her logs")

	return nil
}
