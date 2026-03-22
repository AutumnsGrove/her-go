package cmd

import (
	"fmt"
	"os/exec"

	"github.com/spf13/cobra"
)

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the launchd service",
	RunE:  runStart,
}

func init() {
	rootCmd.AddCommand(startCmd)
}

func runStart(cmd *cobra.Command, args []string) error {
	dest, err := plistPath()
	if err != nil {
		return err
	}

	out, err := exec.Command("launchctl", "load", dest).CombinedOutput()
	if err != nil {
		// launchctl returns an error if the service is already loaded.
		fmt.Printf("Service may already be running: %s\n", string(out))
		return nil
	}

	fmt.Println("Service started.")
	return nil
}
