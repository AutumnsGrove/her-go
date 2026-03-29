package cmd

import (
	"fmt"
	"os/exec"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/spf13/cobra"
)

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print build version and staleness info",
	Run:   runVersion,
}

func init() {
	rootCmd.AddCommand(versionCmd)
}

func runVersion(cmd *cobra.Command, args []string) {
	// ReadBuildInfo returns metadata that Go embeds at compile time.
	// The Settings slice contains VCS info when built with `go build`
	// from a git repo — no -ldflags needed.
	info, ok := debug.ReadBuildInfo()
	if !ok {
		fmt.Println("her (build info unavailable)")
		return
	}

	// Pull VCS settings out of the build info. These are key/value
	// pairs like "vcs.revision" and "vcs.time" — Go populates them
	// automatically from git when you run `go build`.
	var revision, buildTime string
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			revision = s.Value
		case "vcs.time":
			buildTime = s.Value
		}
	}

	// Short hash for display (same 7-char prefix as `git log --oneline`).
	shortRev := revision
	if len(shortRev) > 7 {
		shortRev = shortRev[:7]
	}

	fmt.Println("her v0.0.0-dev")
	if shortRev != "" {
		fmt.Printf("  commit:  %s\n", shortRev)
	}
	if buildTime != "" {
		fmt.Printf("  built:   %s\n", formatBuildTime(buildTime))
	}
	fmt.Printf("  go:      %s\n", runtime.Version())

	// Staleness check: compare the build commit against HEAD.
	if revision != "" {
		checkStaleness(revision, shortRev)
	}
}

// formatBuildTime trims the timezone suffix from the ISO 8601 timestamp
// that Go embeds (e.g. "2026-03-29T15:03:46Z" → "2026-03-29 15:03:46").
func formatBuildTime(t string) string {
	t = strings.TrimSuffix(t, "Z")
	t = strings.Replace(t, "T", " ", 1)
	return t
}

// checkStaleness compares the compiled-in commit against the repo's HEAD.
// If git isn't available or we're not in a repo, it silently does nothing —
// this is intentional so the version command works everywhere.
func checkStaleness(buildRev, shortRev string) {
	// Get HEAD's short hash. If this fails (no git, not a repo), bail quietly.
	headBytes, err := exec.Command("git", "rev-parse", "--short", "HEAD").Output()
	if err != nil {
		return
	}
	head := strings.TrimSpace(string(headBytes))

	// If build revision starts with HEAD, they match — binary is current.
	if strings.HasPrefix(buildRev, head) || strings.HasPrefix(head, shortRev) {
		return
	}

	// Count how many commits the binary is behind HEAD.
	// git rev-list --count <from>..<to> counts commits reachable from <to>
	// but not from <from> — i.e., "how many commits ahead is HEAD?"
	countBytes, err := exec.Command("git", "rev-list", "--count", buildRev+"..HEAD").Output()
	if err != nil {
		// Can't count (maybe build commit was rebased away). Still warn.
		fmt.Printf("\n  ⚠ binary was built from %s, HEAD is now %s\n", shortRev, head)
		fmt.Println("    run 'her install --source' to update")
		return
	}

	count := strings.TrimSpace(string(countBytes))
	if count == "0" {
		return
	}

	// Pluralize "commit" / "commits".
	noun := "commits"
	if count == "1" {
		noun = "commit"
	}

	fmt.Printf("\n  ⚠ binary is %s %s behind HEAD (%s → %s)\n", count, noun, shortRev, head)
	fmt.Println("    run 'her install --source' to update")
}
