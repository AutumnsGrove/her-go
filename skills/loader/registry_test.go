package loader

import (
	"os"
	"path/filepath"
	"testing"
)

// makeTestSkillsDir creates a temporary skills directory with the given
// skill definitions. Each key is a directory name, each value is the
// skill.md content.
//
// This is a common Go test pattern — create a minimal fixture on disk,
// test against it, and let t.TempDir() clean it up automatically.
// In Python you'd use pytest's tmp_path fixture.
func makeTestSkillsDir(t *testing.T, skills map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range skills {
		skillDir := filepath.Join(dir, name)
		if err := os.MkdirAll(skillDir, 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(skillDir, "skill.md"), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

// TestRegistryLoad verifies that the registry discovers and loads skills.
func TestRegistryLoad(t *testing.T) {
	dir := makeTestSkillsDir(t, map[string]string{
		"weather": `---
name: weather
description: "Get weather forecasts"
language: go
---
Check the weather.
`,
		"search": `---
name: search
description: "Search the web"
language: go
---
Search things.
`,
	})

	// No embed client — we're testing discovery, not search.
	reg := NewRegistry(dir, nil)
	count, err := reg.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if count != 2 {
		t.Errorf("Load() = %d, want 2", count)
	}

	// Verify both skills are accessible by name.
	if reg.Get("weather") == nil {
		t.Error("Get(weather) returned nil")
	}
	if reg.Get("search") == nil {
		t.Error("Get(search) returned nil")
	}
	if reg.Get("nonexistent") != nil {
		t.Error("Get(nonexistent) should be nil")
	}
}

// TestRegistryLoadSkipsNonSkillDirs verifies that directories without
// a skill.md are silently skipped (like skillkit/).
func TestRegistryLoadSkipsNonSkillDirs(t *testing.T) {
	dir := t.TempDir()

	// Create a skill directory.
	skillDir := filepath.Join(dir, "weather")
	os.MkdirAll(skillDir, 0755)
	os.WriteFile(filepath.Join(skillDir, "skill.md"), []byte(`---
name: weather
description: "Weather"
language: go
---
`), 0644)

	// Create a non-skill directory (like skillkit/).
	os.MkdirAll(filepath.Join(dir, "skillkit", "go"), 0755)
	os.WriteFile(filepath.Join(dir, "skillkit", "go", "output.go"), []byte("package skillkit"), 0644)

	// Create a plain file (not a directory).
	os.WriteFile(filepath.Join(dir, "README.md"), []byte("# Skills"), 0644)

	reg := NewRegistry(dir, nil)
	count, err := reg.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if count != 1 {
		t.Errorf("Load() = %d, want 1 (should skip non-skill dirs)", count)
	}
}

// TestRegistryLoadSkipsUnmetRequirements verifies that skills with
// unmet requirements are excluded from the registry.
func TestRegistryLoadSkipsUnmetRequirements(t *testing.T) {
	dir := makeTestSkillsDir(t, map[string]string{
		"available": `---
name: available
description: "This skill has no requirements"
language: go
---
Works everywhere.
`,
		"unavailable": `---
name: unavailable
description: "This skill needs a missing env var"
language: go
requires:
  env: [SKILL_TEST_MISSING_ENV_VAR_XYZ]
---
Needs special setup.
`,
	})

	reg := NewRegistry(dir, nil)
	count, err := reg.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if count != 1 {
		t.Errorf("Load() = %d, want 1 (unavailable should be skipped)", count)
	}
	if reg.Get("available") == nil {
		t.Error("available skill should be registered")
	}
	if reg.Get("unavailable") != nil {
		t.Error("unavailable skill should NOT be registered")
	}
}

// TestRegistryList verifies that List returns sorted skill names.
func TestRegistryList(t *testing.T) {
	dir := makeTestSkillsDir(t, map[string]string{
		"charlie": `---
name: charlie
description: "Third"
language: go
---
`,
		"alpha": `---
name: alpha
description: "First"
language: go
---
`,
		"bravo": `---
name: bravo
description: "Second"
language: go
---
`,
	})

	reg := NewRegistry(dir, nil)
	reg.Load()

	names := reg.List()
	if len(names) != 3 {
		t.Fatalf("List() returned %d names, want 3", len(names))
	}
	if names[0] != "alpha" || names[1] != "bravo" || names[2] != "charlie" {
		t.Errorf("List() = %v, want [alpha bravo charlie]", names)
	}
}

// TestRegistryCount verifies the Count method.
func TestRegistryCount(t *testing.T) {
	dir := makeTestSkillsDir(t, map[string]string{
		"one": `---
name: one
description: "First"
language: go
---
`,
		"two": `---
name: two
description: "Second"
language: go
---
`,
	})

	reg := NewRegistry(dir, nil)
	if reg.Count() != 0 {
		t.Error("Count should be 0 before Load")
	}

	reg.Load()
	if reg.Count() != 2 {
		t.Errorf("Count = %d, want 2", reg.Count())
	}
}

// TestRegistryLoadEmptyDir verifies graceful handling of an empty
// skills directory.
func TestRegistryLoadEmptyDir(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry(dir, nil)
	count, err := reg.Load()
	if err != nil {
		t.Fatalf("Load() error: %v", err)
	}
	if count != 0 {
		t.Errorf("Load() = %d, want 0", count)
	}
}

// TestRegistryReload verifies that Load can be called multiple times
// to refresh the registry (e.g., when skills change on disk).
func TestRegistryReload(t *testing.T) {
	dir := makeTestSkillsDir(t, map[string]string{
		"initial": `---
name: initial
description: "The first skill"
language: go
---
`,
	})

	reg := NewRegistry(dir, nil)
	reg.Load()
	if reg.Count() != 1 {
		t.Fatalf("first Load: count = %d, want 1", reg.Count())
	}

	// Add a second skill on disk.
	newDir := filepath.Join(dir, "added")
	os.MkdirAll(newDir, 0755)
	os.WriteFile(filepath.Join(newDir, "skill.md"), []byte(`---
name: added
description: "A new skill"
language: go
---
`), 0644)

	// Reload.
	count, err := reg.Load()
	if err != nil {
		t.Fatalf("reload error: %v", err)
	}
	if count != 2 {
		t.Errorf("reload: count = %d, want 2", count)
	}
}
