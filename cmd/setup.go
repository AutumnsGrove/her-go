package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
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
	Short: "Build the binary and install the launchd service",
	Long: `Does everything needed to install Mira as a launchd service:

  1. Build the binary (go build)
  2. Generate a plist file for the current machine
  3. Create the logs directory
  4. Install the plist to ~/Library/LaunchAgents
  5. Load the service with launchctl`,
	RunE: runSetup,
}

func init() {
	rootCmd.AddCommand(setupCmd)
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

	// Step 1: Build the binary.
	fmt.Println("[1/5] Building binary...")
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
	fmt.Println("[2/5] Generating plist...")
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
	fmt.Println("[3/5] Creating logs directory...")
	if err := os.MkdirAll(logsDir, 0o755); err != nil {
		return fmt.Errorf("failed to create logs directory: %w", err)
	}
	fmt.Printf("      Dir:   %s\n\n", logsDir)

	// Step 4: (plist already written to ~/Library/LaunchAgents in step 2)
	fmt.Println("[4/5] Plist installed to ~/Library/LaunchAgents")
	fmt.Println()

	// Step 5: Load the service.
	fmt.Println("[5/5] Loading service...")
	loadCmd := exec.Command("launchctl", "load", dest)
	loadCmd.Stdout = os.Stdout
	loadCmd.Stderr = os.Stderr
	if err := loadCmd.Run(); err != nil {
		log.Warn("launchctl load failed", "err", err)
		fmt.Println("      You may need to unload first: her stop")
	} else {
		fmt.Println("      Service loaded!")
	}

	// Summary.
	fmt.Println()
	fmt.Println("--- Setup complete ---")
	fmt.Printf("  Binary:   %s\n", binaryPath)
	fmt.Printf("  Plist:    %s\n", dest)
	fmt.Printf("  Logs:     %s\n", logsDir)
	fmt.Printf("  Service:  %s\n", serviceLabel)
	fmt.Println()
	fmt.Println("Commands:")
	fmt.Println("  her start    — start the service")
	fmt.Println("  her stop     — stop the service")
	fmt.Println("  her status   — check service status")
	fmt.Println("  her logs     — tail log output")

	return nil
}
