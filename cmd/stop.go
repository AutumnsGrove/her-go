package cmd

import (
	"fmt"

	"github.com/spf13/cobra"
	"her/procmgr"
)

var stopCmd = &cobra.Command{
	Use:   "stop",
	Short: "Stop the system service",
	RunE:  runStop,
}

func init() {
	rootCmd.AddCommand(stopCmd)
}

func runStop(cmd *cobra.Command, args []string) error {
	label, err := loadServiceLabel()
	if err != nil {
		return err
	}

	mgr, err := procmgr.New(label)
	if err != nil {
		return err
	}

	if err := mgr.Stop(); err != nil {
		fmt.Printf("Failed to stop service: %v\n", err)
		return err
	}

	fmt.Println("Service stopped.")
	return nil
}
