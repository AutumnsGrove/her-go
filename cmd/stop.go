package cmd

import (
	"fmt"
	"os/exec"

	"github.com/spf13/cobra"
)

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the launchd service",
	RunE:  runStop,
}

func init() {
	rootCmd.AddCommand(stopCmd)
}

func runStop(cmd *cobra.Command, args []string) error {
	dest, err := plistPath()
	if err != nil {
		return err
	}

	out, err := exec.Command("launchctl", "unload", dest).CombinedOutput()
	if err != nil {
		fmt.Printf("Failed to stop service: %s\n", string(out))
		return fmt.Errorf("launchctl unload failed: %w", err)
	}

	fmt.Println("Service stopped.")
	return nil
}
