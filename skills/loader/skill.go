// Package loader discovers, parses, and registers skills from the filesystem.
//
// A skill is a self-contained directory under skills/ with a skill.md file
// that describes what it does, what parameters it accepts, and what permissions
// it needs. This package reads those files, validates requirements, and builds
// an in-memory registry the agent can search.
//
// Think of it like a plugin loader — it finds plugins on disk, checks they're
// compatible, and makes them available to the system. In Python, this is
// similar to what setuptools entry_points or pluggy do, but simpler.
package loader

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

// Skill holds everything parsed from a skill.md file. The frontmatter
// becomes struct fields; the markdown body becomes Instructions.
//
// The agent never sees all of this at once — find_skill returns just the
// name, description, and score. Only when the agent calls run_skill does
// the full Instructions get loaded into context.
type Skill struct {
	// --- Identity ---
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Version     string `yaml:"version"`
	Language    string `yaml:"language"` // "go" or "python"
	Author      string `yaml:"author"`

	// --- Trust ---
	Hash string `yaml:"hash"` // SHA256 of source file(s), set by the author

	// --- Parameters ---
	Params []Param `yaml:"params"`

	// --- Permissions (enforced by sandbox) ---
	Permissions Permissions `yaml:"permissions"`

	// --- Requirements (skill hidden if not met) ---
	Requires Requirements `yaml:"requires"`

	// --- Populated by the loader, not from YAML ---
	Instructions string     `yaml:"-"` // markdown body (below the frontmatter)
	Dir          string     `yaml:"-"` // absolute path to the skill directory
	TrustLevel   TrustLevel `yaml:"-"` // resolved by ResolveTrust at load time
}

// Param describes a single parameter the skill accepts.
// The agent sees these when deciding how to call the skill.
//
// This is similar to how OpenAI function-calling tools define parameters —
// name, type, required, description. The agent uses this schema to build
// the JSON it pipes to the skill's stdin.
type Param struct {
	Name        string `yaml:"name"`
	Type        string `yaml:"type"` // "string", "int", "bool", "float"
	Required    bool   `yaml:"required"`
	Default     any    `yaml:"default"`
	Description string `yaml:"description"`
}

// Permissions declares what the skill is allowed to do. The sandbox
// enforces these — a skill that tries to access something not listed
// here gets blocked.
type Permissions struct {
	Network bool     `yaml:"network"`
	Domains []string `yaml:"domains"` // allowlisted domains (proxy-enforced)
	FS      []string `yaml:"fs"`      // allowed filesystem paths (relative to skill dir)
	Env     []string `yaml:"env"`     // env vars the skill needs
	Timeout string   `yaml:"timeout"` // max execution time (e.g., "30s")
}

// Requirements are checked at startup. If any requirement isn't met,
// the skill is hidden from the agent entirely — it won't appear in
// find_skill results. This prevents the agent from trying to use
// something that can't possibly work.
type Requirements struct {
	Env  []string `yaml:"env"`  // env vars that must be set
	Bins []string `yaml:"bins"` // binaries that must be on PATH
	OS   []string `yaml:"os"`   // supported platforms (linux, darwin, windows)
}

// ParseSkillFile reads a skill.md file and returns a parsed Skill.
//
// The file format is YAML frontmatter (between --- delimiters) followed
// by a markdown body. This is the same format used by Hugo, Jekyll, and
// other static site generators — it's a well-known pattern for putting
// machine-readable metadata alongside human-readable content.
//
// Example:
//
//	---
//	name: web_search
//	description: "Search the web using Tavily API"
//	language: go
//	params:
//	  - name: query
//	    type: string
//	    required: true
//	---
//	## Instructions
//	Search the web for current information...
func ParseSkillFile(path string) (*Skill, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading skill file: %w", err)
	}

	frontmatter, body, err := splitFrontmatter(data)
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	var skill Skill
	if err := yaml.Unmarshal(frontmatter, &skill); err != nil {
		return nil, fmt.Errorf("parsing frontmatter in %s: %w", path, err)
	}

	skill.Instructions = strings.TrimSpace(body)
	skill.Dir = filepath.Dir(path)

	if skill.Name == "" {
		return nil, fmt.Errorf("skill at %s has no name", path)
	}

	return &skill, nil
}

// splitFrontmatter separates YAML frontmatter from the markdown body.
// Frontmatter must be enclosed in --- delimiters at the start of the file.
//
// Returns (frontmatter bytes, body string, error).
func splitFrontmatter(data []byte) ([]byte, string, error) {
	// Trim any leading whitespace/BOM.
	content := bytes.TrimLeft(data, "\xef\xbb\xbf \t\r\n")

	if !bytes.HasPrefix(content, []byte("---")) {
		return nil, "", fmt.Errorf("missing opening --- delimiter")
	}

	// Skip past the opening ---.
	content = content[3:]

	// Find the closing ---.
	idx := bytes.Index(content, []byte("\n---"))
	if idx < 0 {
		return nil, "", fmt.Errorf("missing closing --- delimiter")
	}

	frontmatter := content[:idx]
	body := string(content[idx+4:]) // skip past \n---

	return frontmatter, body, nil
}

// MeetsRequirements checks whether the current system satisfies the
// skill's requirements. Returns true if all requirements are met,
// or a description of what's missing.
//
// This is checked at startup for each skill. Failed requirements mean
// the skill is completely invisible to the agent — not "disabled",
// just absent. The agent can't be confused by skills it can't use.
func (s *Skill) MeetsRequirements() (bool, string) {
	// Check OS.
	if len(s.Requires.OS) > 0 {
		currentOS := runtime.GOOS
		found := false
		for _, os := range s.Requires.OS {
			if os == currentOS {
				found = true
				break
			}
		}
		if !found {
			return false, fmt.Sprintf("requires OS %v, running %s", s.Requires.OS, currentOS)
		}
	}

	// Check env vars.
	for _, env := range s.Requires.Env {
		if os.Getenv(env) == "" {
			return false, fmt.Sprintf("requires env var %s", env)
		}
	}

	// Check binaries on PATH.
	// exec.LookPath is Go's equivalent of Python's shutil.which() —
	// it searches PATH for a named executable.
	for _, bin := range s.Requires.Bins {
		if _, err := exec.LookPath(bin); err != nil {
			return false, fmt.Sprintf("requires binary %s on PATH", bin)
		}
	}

	return true, ""
}
