package loader

import (
	"os"
	"path/filepath"
	"testing"
)

// sampleSkillMD is a realistic skill.md for testing.
// It exercises all frontmatter fields and has a markdown body.
const sampleSkillMD = `---
name: web_search
description: "Search the web for current information using the Tavily API"
version: "1.0.0"
language: go
author: autumn
hash: "sha256:abc123"
params:
  - name: query
    type: string
    required: true
    description: "The search query"
  - name: limit
    type: int
    required: false
    default: 5
    description: "Maximum number of results"
permissions:
  network: true
  domains:
    - api.tavily.com
  fs:
    - refs/
  env:
    - TAVILY_API_KEY
  timeout: 30s
requires:
  env: []
  bins: []
  os: []
---

## Instructions

Search the web for current information. Use when the user asks about
recent events or needs up-to-date data.
`

// TestParseSkillFile verifies that a full skill.md is parsed correctly.
func TestParseSkillFile(t *testing.T) {
	// Write the sample to a temp file.
	dir := t.TempDir()
	path := filepath.Join(dir, "skill.md")
	if err := os.WriteFile(path, []byte(sampleSkillMD), 0644); err != nil {
		t.Fatal(err)
	}

	skill, err := ParseSkillFile(path)
	if err != nil {
		t.Fatalf("ParseSkillFile() error: %v", err)
	}

	// Check identity fields.
	if skill.Name != "web_search" {
		t.Errorf("Name = %q, want %q", skill.Name, "web_search")
	}
	if skill.Description != "Search the web for current information using the Tavily API" {
		t.Errorf("Description = %q", skill.Description)
	}
	if skill.Version != "1.0.0" {
		t.Errorf("Version = %q", skill.Version)
	}
	if skill.Language != "go" {
		t.Errorf("Language = %q", skill.Language)
	}
	if skill.Author != "autumn" {
		t.Errorf("Author = %q", skill.Author)
	}
	if skill.Hash != "sha256:abc123" {
		t.Errorf("Hash = %q", skill.Hash)
	}

	// Check params.
	if len(skill.Params) != 2 {
		t.Fatalf("got %d params, want 2", len(skill.Params))
	}
	if skill.Params[0].Name != "query" || !skill.Params[0].Required {
		t.Errorf("param[0] = %+v", skill.Params[0])
	}
	if skill.Params[1].Name != "limit" || skill.Params[1].Required {
		t.Errorf("param[1] = %+v", skill.Params[1])
	}

	// Check permissions.
	if !skill.Permissions.Network {
		t.Error("Permissions.Network should be true")
	}
	if len(skill.Permissions.Domains) != 1 || skill.Permissions.Domains[0] != "api.tavily.com" {
		t.Errorf("Permissions.Domains = %v", skill.Permissions.Domains)
	}
	if skill.Permissions.Timeout != "30s" {
		t.Errorf("Permissions.Timeout = %q", skill.Permissions.Timeout)
	}

	// Check instructions (markdown body).
	if skill.Instructions == "" {
		t.Error("Instructions should not be empty")
	}
	if skill.Instructions[:16] != "## Instructions\n" {
		t.Errorf("Instructions starts with %q", skill.Instructions[:16])
	}

	// Check Dir is set.
	if skill.Dir != dir {
		t.Errorf("Dir = %q, want %q", skill.Dir, dir)
	}
}

// TestParseSkillFileMissingName verifies that a skill without a name
// produces a clear error.
func TestParseSkillFileMissingName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "skill.md")
	content := `---
description: "A skill with no name"
language: go
---
## Instructions
Do things.
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := ParseSkillFile(path)
	if err == nil {
		t.Fatal("expected error for skill with no name")
	}
}

// TestParseSkillFileBadFrontmatter verifies error on malformed YAML.
func TestParseSkillFileBadFrontmatter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "skill.md")
	content := `---
name: [this is invalid yaml
---
## Instructions
`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := ParseSkillFile(path)
	if err == nil {
		t.Fatal("expected error for bad frontmatter")
	}
}

// TestParseSkillFileNoDelimiters verifies error when --- is missing.
func TestParseSkillFileNoDelimiters(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "skill.md")
	content := `Just a plain markdown file with no frontmatter.`
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatal(err)
	}

	_, err := ParseSkillFile(path)
	if err == nil {
		t.Fatal("expected error for missing frontmatter")
	}
}

// TestMeetsRequirementsAllEmpty verifies that a skill with no requirements
// passes on any system.
func TestMeetsRequirementsAllEmpty(t *testing.T) {
	skill := &Skill{
		Name:     "test",
		Requires: Requirements{},
	}
	ok, reason := skill.MeetsRequirements()
	if !ok {
		t.Errorf("empty requirements should pass, got: %s", reason)
	}
}

// TestMeetsRequirementsEnvMissing verifies that a missing env var
// causes the check to fail.
func TestMeetsRequirementsEnvMissing(t *testing.T) {
	skill := &Skill{
		Name: "test",
		Requires: Requirements{
			Env: []string{"DEFINITELY_NOT_SET_XYZ_12345"},
		},
	}
	ok, reason := skill.MeetsRequirements()
	if ok {
		t.Error("should fail when required env var is missing")
	}
	if reason == "" {
		t.Error("reason should explain what's missing")
	}
}

// TestMeetsRequirementsBinMissing verifies that a missing binary
// causes the check to fail.
func TestMeetsRequirementsBinMissing(t *testing.T) {
	skill := &Skill{
		Name: "test",
		Requires: Requirements{
			Bins: []string{"definitely_not_a_real_binary_xyz"},
		},
	}
	ok, _ := skill.MeetsRequirements()
	if ok {
		t.Error("should fail when required binary is missing")
	}
}

// TestMeetsRequirementsOSMatch verifies OS checking works for the
// current platform.
func TestMeetsRequirementsOSMatch(t *testing.T) {
	// This should pass — we're running on this OS right now.
	skill := &Skill{
		Name: "test",
		Requires: Requirements{
			OS: []string{"darwin", "linux"},
		},
	}
	ok, _ := skill.MeetsRequirements()
	if !ok {
		t.Error("should pass when current OS is in the list")
	}
}

// TestMeetsRequirementsOSMismatch verifies that an unsupported OS
// causes the check to fail.
func TestMeetsRequirementsOSMismatch(t *testing.T) {
	skill := &Skill{
		Name: "test",
		Requires: Requirements{
			OS: []string{"plan9"}, // sorry, no Plan 9 today
		},
	}
	ok, _ := skill.MeetsRequirements()
	if ok {
		t.Error("should fail when current OS is not in the list")
	}
}
