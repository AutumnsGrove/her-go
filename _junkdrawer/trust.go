package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"her/skills/loader"

	"github.com/spf13/cobra"
)

// trustSignWrite controls whether `her trust sign` updates skill.md in place.
var trustSignWrite bool

// trustCmd lists all skills with their trust tiers.
//
// This walks the skills directory directly rather than using the registry,
// so it shows ALL skills — even ones that fail requirements checks
// (e.g., missing API key). You always want to see trust status.
var trustCmd = &cobra.Command{
	Use:   "trust",
	Short: "Show trust tiers for all skills",
	Long: `Lists every skill in the skills/ directory with its trust tier,
determined by SHA256 hash verification of the source file.

Trust tiers:
  2nd-party   Hash in skill.md matches source on disk (vetted by you)
  3rd-party   Hash exists but doesn't match (agent modified it)
  4th-party   No hash in skill.md (never signed)`,
	RunE: runTrust,
}

// trustSignCmd computes the SHA256 hash for a skill's source file.
// With --write, it updates the hash field in skill.md.
var trustSignCmd = &cobra.Command{
	Use:   "sign [skill-name]",
	Short: "Compute and optionally write the trust hash for a skill",
	Long: `Computes the SHA256 hash of a skill's main source file (main.go or
main.py) and prints it. With --write, updates the hash: field in the
skill's skill.md file, promoting the skill to 2nd-party trust.

This is how you "vet" a skill — review the source, then sign it.`,
	Args: cobra.ExactArgs(1),
	RunE: runTrustSign,
}

func init() {
	trustSignCmd.Flags().BoolVar(&trustSignWrite, "write", false, "update skill.md with the computed hash")
	trustCmd.AddCommand(trustSignCmd)
	rootCmd.AddCommand(trustCmd)
}

// skillsDir returns the path to the skills/ directory relative to the config file.
func skillsDir() string {
	return filepath.Join(filepath.Dir(cfgFile), "skills")
}

// runTrust lists all skills with their trust info.
func runTrust(cmd *cobra.Command, args []string) error {
	dir := skillsDir()

	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("reading skills directory: %w", err)
	}

	// Header
	fmt.Printf("%-20s %-12s %-10s %s\n", "SKILL", "TRUST", "LANG", "HASH")
	fmt.Println(strings.Repeat("-", 75))

	found := 0
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		// Skip non-skill directories (skillkit, etc.)
		skillPath := filepath.Join(dir, entry.Name(), "skill.md")
		if _, err := os.Stat(skillPath); err != nil {
			continue
		}

		skill, err := loader.ParseSkillFile(skillPath)
		if err != nil {
			fmt.Printf("%-20s %-12s %-10s %s\n", entry.Name(), "ERROR", "", err.Error())
			continue
		}

		// Resolve trust.
		skill.TrustLevel = loader.ResolveTrust(skill)

		// Format the hash column.
		hashDisplay := "(none)"
		if skill.Hash != "" {
			// Truncate the hex part for display — full hash is long.
			hashDisplay = truncateHash(skill.Hash)
			if skill.TrustLevel == loader.TrustSecondParty {
				hashDisplay += " OK"
			} else if skill.TrustLevel == loader.TrustThirdParty {
				hashDisplay += " MISMATCH"
			}
		}

		fmt.Printf("%-20s %-12s %-10s %s\n",
			skill.Name,
			skill.TrustLevel,
			skill.Language,
			hashDisplay,
		)
		found++
	}

	if found == 0 {
		fmt.Println("No skills found.")
	}

	return nil
}

// runTrustSign computes the hash for a skill and optionally writes it.
func runTrustSign(cmd *cobra.Command, args []string) error {
	skillName := args[0]
	dir := skillsDir()

	skillPath := filepath.Join(dir, skillName, "skill.md")
	skill, err := loader.ParseSkillFile(skillPath)
	if err != nil {
		return fmt.Errorf("parsing %s: %w", skillPath, err)
	}

	hash, err := loader.ComputeSourceHash(skill)
	if err != nil {
		return fmt.Errorf("computing hash: %w", err)
	}

	fmt.Printf("Skill:    %s\n", skill.Name)
	fmt.Printf("Language: %s\n", skill.Language)
	fmt.Printf("Hash:     %s\n", hash)

	if skill.Hash != "" {
		if skill.Hash == hash {
			fmt.Println("Status:   hash matches (already 2nd-party)")
		} else {
			fmt.Println("Status:   hash MISMATCH (was modified)")
			fmt.Printf("Old hash: %s\n", skill.Hash)
		}
	} else {
		fmt.Println("Status:   no hash set (currently 4th-party)")
	}

	if !trustSignWrite {
		fmt.Println("\nRun with --write to update skill.md")
		return nil
	}

	// Update the hash in skill.md.
	if err := writeHashToSkillMD(skillPath, hash); err != nil {
		return fmt.Errorf("writing hash: %w", err)
	}

	fmt.Printf("\nUpdated %s with new hash.\n", skillPath)
	return nil
}

// writeHashToSkillMD updates the hash: field in a skill.md file in place.
//
// Rather than parsing and re-serializing YAML (which would lose comments
// and formatting), we do a simple string operation:
//   - If a "hash:" line exists in the frontmatter, replace it.
//   - If no "hash:" line exists, insert one after the "author:" line.
//
// This is intentionally simple — skill.md files have a predictable format.
func writeHashToSkillMD(path, hash string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	content := string(data)
	newLine := fmt.Sprintf("hash: %q", hash)

	// Check if there's already a hash: line.
	if strings.Contains(content, "hash:") {
		// Replace the existing hash line. Find the line and swap it.
		lines := strings.Split(content, "\n")
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "hash:") {
				lines[i] = newLine
				break
			}
		}
		content = strings.Join(lines, "\n")
	} else {
		// Insert after the author: line. If no author line, insert after
		// the language: line. Fallback: insert before the first ---.
		lines := strings.Split(content, "\n")
		inserted := false
		for i, line := range lines {
			trimmed := strings.TrimSpace(line)
			if strings.HasPrefix(trimmed, "author:") || strings.HasPrefix(trimmed, "language:") {
				// Insert the hash line right after this line.
				rest := make([]string, len(lines[i+1:]))
				copy(rest, lines[i+1:])
				lines = append(lines[:i+1], newLine)
				lines = append(lines, rest...)
				inserted = true
				break
			}
		}
		if !inserted {
			return fmt.Errorf("could not find author: or language: line to insert hash after")
		}
		content = strings.Join(lines, "\n")
	}

	return os.WriteFile(path, []byte(content), 0644)
}

// truncateHash shortens "sha256:abcdef0123456789..." to "sha256:abcdef01..."
// for display in the trust table. The full hash is 64 hex chars — too long.
func truncateHash(hash string) string {
	// Show prefix + first 12 hex chars.
	if len(hash) > 19 {
		return hash[:19] + "..."
	}
	return hash
}
