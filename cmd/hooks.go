package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"
)

var hooksCmd = &cobra.Command{
	Use:   "hooks",
	Short: "Install git pre-commit hooks for this project",
	Long: `Sets up git to use the .githooks/ directory for hook scripts.

This enables pre-commit checks that run automatically before every commit:
  • gofmt    — blocks unformatted code
  • go vet   — catches static analysis issues
  • go build — blocks commits that don't compile
  • mod tidy — ensures go.mod/go.sum are clean
  • secrets  — catches API keys before they hit the repo

Run once after cloning. Bypass individual commits with: git commit --no-verify`,
	RunE: runHooks,
}

func init() {
	rootCmd.AddCommand(hooksCmd)
}

func runHooks(cmd *cobra.Command, args []string) error {
	// Make sure we're in the project root (go.mod exists)
	wd, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("getting working directory: %w", err)
	}

	if _, err := os.Stat(filepath.Join(wd, "go.mod")); os.IsNotExist(err) {
		return fmt.Errorf("not in the her project root (no go.mod found in %s)", wd)
	}

	// Verify .githooks/pre-commit exists
	hookPath := filepath.Join(wd, ".githooks", "pre-commit")
	if _, err := os.Stat(hookPath); os.IsNotExist(err) {
		return fmt.Errorf(".githooks/pre-commit not found — is the repo up to date?")
	}

	// Point git at our .githooks directory
	gitCmd := exec.Command("git", "config", "core.hooksPath", ".githooks")
	gitCmd.Dir = wd
	gitCmd.Stdout = os.Stdout
	gitCmd.Stderr = os.Stderr

	if err := gitCmd.Run(); err != nil {
		return fmt.Errorf("failed to configure git hooks path: %w", err)
	}

	fmt.Println("Hooks installed.")
	fmt.Println("")
	fmt.Println("  Pre-commit checks now run on every commit:")
	fmt.Println("    gofmt, go vet, go build, mod tidy, secrets scan")
	fmt.Println("")
	fmt.Println("  Bypass when needed: git commit --no-verify")

	return nil
}
