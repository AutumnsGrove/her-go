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
	Long: `Rebuilds the her binary from source and drops it in the project directory.
Use this after pulling new code to update the binary.

  her install --source    rebuild from source`,
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

	// Build the binary name from the module name (the Go convention
	// for go install). We build locally with go build instead of
	// go install so the binary lands in the project directory — not
	// buried in GOPATH/bin where it's easy to forget about.
	binName := "her"
	binPath := filepath.Join(wd, binName)

	fmt.Printf("Building %s from source...\n", binName)
	start := time.Now()

	// go build -o ./her . — drops the binary right here in the project.
	buildCmd := exec.Command("go", "build", "-o", binPath, ".")
	buildCmd.Dir = wd
	buildCmd.Stdout = os.Stdout
	buildCmd.Stderr = os.Stderr

	if err := buildCmd.Run(); err != nil {
		return fmt.Errorf("build failed: %w", err)
	}

	elapsed := time.Since(start).Round(time.Millisecond)
	fmt.Printf("Installed %s in %s. Run './her run' to start.\n", binPath, elapsed)
	return nil
}
