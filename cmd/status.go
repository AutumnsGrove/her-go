package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/spf13/cobra"
)

var statusCmd = &cobra.Command{
	Use:   "status",
	Short: "Show launchd service status",
	RunE:  runStatus,
}

func init() {
	rootCmd.AddCommand(statusCmd)
}

func runStatus(cmd *cobra.Command, args []string) error {
	// Get the current user's UID for the launchctl print command.
	uid := os.Getuid()
	target := fmt.Sprintf("gui/%d/%s", uid, serviceLabel)

	out, err := exec.Command("launchctl", "print", target).CombinedOutput()
	if err != nil {
		fmt.Println("Service is not loaded.")
		fmt.Println()
		fmt.Println("Run 'her setup' to install, or 'her start' to load.")
		return nil
	}

	output := string(out)

	// Parse key fields from launchctl print output.
	fmt.Printf("Service: %s\n", serviceLabel)
	fmt.Println()

	// State (e.g., "state = running")
	if val := parseLaunchctlField(output, "state"); val != "" {
		fmt.Printf("  State:       %s\n", val)
	}

	// PID
	if val := parseLaunchctlField(output, "pid"); val != "" {
		fmt.Printf("  PID:         %s\n", val)
	}

	// Last exit status
	if val := parseLaunchctlField(output, "last exit code"); val != "" {
		fmt.Printf("  Last exit:   %s\n", val)
	}

	// Log paths — derive from working directory.
	workDir, _ := os.Getwd()
	fmt.Printf("  Stdout log:  %s/logs/stdout.log\n", workDir)
	fmt.Printf("  Stderr log:  %s/logs/stderr.log\n", workDir)

	// Also show run count if available.
	if val := parseLaunchctlField(output, "runs"); val != "" {
		fmt.Printf("  Run count:   %s\n", val)
	}

	fmt.Println()
	fmt.Println("Use 'her logs' to tail output, 'her stop' to stop.")

	return nil
}

// parseLaunchctlField extracts a value from launchctl print output.
// Lines look like: "    pid = 12345" or "    state = running".
func parseLaunchctlField(output, field string) string {
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, field+" = ") {
			parts := strings.SplitN(trimmed, " = ", 2)
			if len(parts) == 2 {
				return strings.TrimSpace(parts[1])
			}
		}
	}
	return ""
}