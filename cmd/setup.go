package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"text/template"

	"github.com/spf13/cobra"
)

// serviceLabel is the launchd service identifier used across all
// service management commands.
const serviceLabel = "com.mira.her-go"

// plistPath returns the full path to the plist in ~/Library/LaunchAgents.
func plistPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("could not determine home directory: %w", err)
	}
	return filepath.Join(home, "Library", "LaunchAgents", serviceLabel+".plist"), nil
}

// plistData holds the values injected into the plist template.
type plistData struct {
	Label        string
	BinaryPath   string
	WorkDir      string
	StdoutPath   string
	StderrPath   string
	UserName     string
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
        <string>/usr/local/bin:/usr/bin:/bin:/usr/local/go/bin</string>
    </dict>

    <key>UserName</key>
    <string>{{.UserName}}</string>
</dict>
</plist>
`

var setupCmd = &cobra.Command{
	Use:   "setup",
	Short: "Build the binary, install dependencies, and configure the launchd service",
	Long: `Does everything needed to install Mira as a launchd service:

  1. Build the binary (go build)
  2. Install ML dependencies in background (parakeet-mlx, ffmpeg check)
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
	results := make(chan depResult, 4)
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

	// Close the channel once all goroutines finish.
	// This runs in its own goroutine so installDeps() returns immediately.
	go func() {
		wg.Wait()
		close(results)
	}()

	return results
}

func runSetup(cmd *cobra.Command, args []string) error {
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

	fmt.Printf("Setting up Mira on %s as %s\n", hostname, currentUser.Username)
	fmt.Printf("Working directory: %s\n\n", workDir)

	// Kick off dependency installs in the background immediately.
	// These run concurrently while we build the binary and set up launchd.
	depResults := installDeps()

	// Step 1: Build the binary.
	fmt.Println("[1/6] Building binary...")
	binaryPath := filepath.Join(workDir, "her-go")
	buildCmd := exec.Command("go", "build", "-o", binaryPath, ".")
	buildCmd.Dir = workDir
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr
	if err := buildCmd.Run(); err != nil {
		return fmt.Errorf("go build failed: %w", err)
	}
	fmt.Printf("      Built: %s\n\n", binaryPath)

	// Step 2: Generate the plist.
	fmt.Println("[2/6] Generating plist...")
	logsDir := filepath.Join(workDir, "logs")
	data := plistData{
		Label:      serviceLabel,
		BinaryPath: binaryPath,
		WorkDir:    workDir,
		StdoutPath: filepath.Join(logsDir, "stdout.log"),
		StderrPath: filepath.Join(logsDir, "stderr.log"),
		UserName:   currentUser.Username,
	}

	tmpl, err := template.New("plist").Parse(plistTemplate)
	if err != nil {
		return fmt.Errorf("failed to parse plist template: %w", err)
	}

	dest, err := plistPath()
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
	fmt.Printf("      Wrote: %s\n\n", dest)

	// Step 3: Create logs directory.
	fmt.Println("[3/6] Creating logs directory...")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return fmt.Errorf("failed to create logs directory: %w", err)
	}
	fmt.Printf("      Dir:   %s\n\n", logsDir)

	// Step 4: (plist already written to ~/Library/LaunchAgents in step 2)
	fmt.Println("[4/6] Plist installed to ~/Library/LaunchAgents")
	fmt.Println()

	// Step 5: Load the service.
	fmt.Println("[5/6] Loading service...")
	loadCmd := exec.Command("launchctl", "load", dest)
	loadCmd.Stdout = os.Stdout
	loadCmd.Stderr = os.Stderr
	if err := loadCmd.Run(); err != nil {
		log.Warn("launchctl load failed", "err", err)
		fmt.Println("      You may need to unload first: her stop")
	} else {
		fmt.Println("      Service loaded!")
	}
	fmt.Println()

	// Step 6: Wait for background dependency installs to finish.
	// The channel was created in installDeps() — ranging over it gives
	// us each result as it arrives, and stops when the channel closes
	// (which happens after all goroutines call wg.Done()).
	fmt.Println("[6/6] ML dependencies...")
	var warnings []string
	for r := range depResults {
		status := "ok"
		if !r.OK {
			status = "MISSING"
			warnings = append(warnings, fmt.Sprintf("  ! %s: %s", r.Name, r.Message))
		}
		fmt.Printf("      %-20s %s (%s)\n", r.Name, status, r.Message)
	}

	// Summary.
	fmt.Println()
	fmt.Println("--- Setup complete ---")
	fmt.Printf("  Binary:   %s\n", binaryPath)
	fmt.Printf("  Plist:    %s\n", dest)
	fmt.Printf("  Logs:     %s\n", logsDir)
	fmt.Printf("  Service:  %s\n", serviceLabel)

	for _, w := range warnings {
		log.Warn(w)
	}

	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  her start    — start the service")
	fmt.Println("  her stop     — stop the service")
	fmt.Println("  her status   — check service status")
	fmt.Println("  her logs     — tail log output")

	return nil
}
