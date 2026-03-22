package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/spf13/cobra"
)

var installSource bool

var installCmd = &cobra.Command{
	Use:   "install",
	Short: "Rebuild and install the her binary from source",
	Long: `Rebuilds the her binary from the current source directory and installs
it to your GOPATH/bin. Use this after pulling new code to update the
binary without manually running go commands.

  her install --source    rebuild from source and install`,
	RunE: runInstall,
}

func init() {
	installCmd.Flags().BoolVar(&installSource, "source", false, "rebuild from source (required)")
	rootCmd.AddCommand(installCmd)
}

func runInstall(cmd *cobra.Command, args []string) error {
	if !installSource {
		fmt.Println("Use --source to rebuild from source:")
		fmt.Println("  her install --source")
		return nil
	}

	// Find the source directory. If we're running from it, use cwd.
	// Otherwise, check if go.mod exists to confirm we're in the right place.
	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}

	if _, err := os.Stat(filepath.Join(wd, "go.mod")); os.IsNotExist(err) {
		return fmt.Errorf("not in the her source directory (no go.mod found in %s)", wd)
	}

	fmt.Println("Building and installing from source...")
	start := time.Now()

	// go install . builds the binary and puts it in GOPATH/bin.
	buildCmd := exec.Command("go", "install", ".")
	buildCmd.Dir = wd
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr

	if err := buildCmd.Run(); err != nil {
		return fmt.Errorf("build failed: %w", err)
	}

	elapsed := time.Since(start).Round(time.Millisecond)
	fmt.Printf("Installed in %s. Run 'her run' to start.\n", elapsed)
	return nil
}
