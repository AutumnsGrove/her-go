package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"her/procmgr"
)

var startCmd = &cobra.Command{
	Use:   "start",
	Short: "Start the system service",
	Long:  "Starts her-go as a system service (launchd on macOS, systemd on Linux). If setup hasn't been run yet, runs it automatically first.",
	RunE:  runStart,
}

func init() {
	rootCmd.AddCommand(startCmd)
}

func runStart(cmd *cobra.Command, args []string) error {
	label, err := loadServiceLabel()
	if err != nil {
		return err
	}

	mgr, err := procmgr.New(label)
	if err != nil {
		return err
	}

	if err := mgr.Start(); err != nil {
		// If the service isn't installed yet, run setup first.
		fmt.Println("Service not set up yet. Running setup first...")
		fmt.Println()
		if err := runSetup(cmd, args); err != nil {
			return fmt.Errorf("setup failed: %w", err)
		}
		return nil
	}

	fmt.Println("Service started.")
	return nil
}
