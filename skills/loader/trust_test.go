package loader

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// TrustLevel.String()
// ---------------------------------------------------------------------------

func TestTrustLevelString(t *testing.T) {
	// Table-driven test — a Go convention where you define test cases as
	// a slice of structs and loop over them. Keeps tests compact when
	// you're testing the same function with different inputs.
	tests := []struct {
		level TrustLevel
		want  string
	}{
		{TrustFirstParty, "1st-party"},
		{TrustSecondParty, "2nd-party"},
		{TrustThirdParty, "3rd-party"},
		{TrustFourthParty, "4th-party"},
		{TrustLevel(99), "unknown(99)"},
	}

	for _, tt := range tests {
		if got := tt.level.String(); got != tt.want {
			t.Errorf("TrustLevel(%d).String() = %q, want %q", int(tt.level), got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// TrustLevel.MaxTimeout()
// ---------------------------------------------------------------------------

func TestTrustLevelMaxTimeout(t *testing.T) {
	tests := []struct {
		level TrustLevel
		want  time.Duration
	}{
		{TrustSecondParty, 30 * time.Second},
		{TrustThirdParty, 10 * time.Second},
		{TrustFourthParty, 5 * time.Second},
	}

	for _, tt := range tests {
		if got := tt.level.MaxTimeout(); got != tt.want {
			t.Errorf("%s.MaxTimeout() = %v, want %v", tt.level, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// TrustLevel.AllowDirectNetwork()
// ---------------------------------------------------------------------------

func TestTrustLevelAllowDirectNetwork(t *testing.T) {
	tests := []struct {
		level TrustLevel
		want  bool
	}{
		{TrustFirstParty, true},
		{TrustSecondParty, true},
		{TrustThirdParty, false},
		{TrustFourthParty, false},
	}

	for _, tt := range tests {
		if got := tt.level.AllowDirectNetwork(); got != tt.want {
			t.Errorf("%s.AllowDirectNetwork() = %v, want %v", tt.level, got, tt.want)
		}
	}
}

// ---------------------------------------------------------------------------
// ComputeSourceHash
// ---------------------------------------------------------------------------

// knownHash pre-computes a SHA256 hash so we can verify ComputeSourceHash
// against a known-good value. Same as running: echo -n "content" | sha256sum
func knownHash(content string) string {
	h := sha256.Sum256([]byte(content))
	return "sha256:" + hex.EncodeToString(h[:])
}

func TestComputeSourceHashGo(t *testing.T) {
	dir := t.TempDir()
	content := "package main\n\nfunc main() {}\n"
	os.WriteFile(filepath.Join(dir, "main.go"), []byte(content), 0644)

	skill := &Skill{Language: "go", Dir: dir}
	got, err := ComputeSourceHash(skill)
	if err != nil {
		t.Fatalf("ComputeSourceHash() error: %v", err)
	}

	want := knownHash(content)
	if got != want {
		t.Errorf("ComputeSourceHash() = %q, want %q", got, want)
	}
}

func TestComputeSourceHashPython(t *testing.T) {
	dir := t.TempDir()
	content := "print('hello')\n"
	os.WriteFile(filepath.Join(dir, "main.py"), []byte(content), 0644)

	skill := &Skill{Language: "python", Dir: dir}
	got, err := ComputeSourceHash(skill)
	if err != nil {
		t.Fatalf("ComputeSourceHash() error: %v", err)
	}

	want := knownHash(content)
	if got != want {
		t.Errorf("ComputeSourceHash() = %q, want %q", got, want)
	}
}

func TestComputeSourceHashMissingFile(t *testing.T) {
	dir := t.TempDir()
	// No main.go written — should fail.
	skill := &Skill{Language: "go", Dir: dir}

	_, err := ComputeSourceHash(skill)
	if err == nil {
		t.Fatal("ComputeSourceHash() expected error for missing file, got nil")
	}
}

func TestComputeSourceHashUnsupportedLanguage(t *testing.T) {
	skill := &Skill{Language: "rust", Dir: t.TempDir()}

	_, err := ComputeSourceHash(skill)
	if err == nil {
		t.Fatal("ComputeSourceHash() expected error for unsupported language, got nil")
	}
}

// ---------------------------------------------------------------------------
// ResolveTrust
// ---------------------------------------------------------------------------

func TestResolveTrustSecondParty(t *testing.T) {
	dir := t.TempDir()
	content := "package main\n\nfunc main() {}\n"
	os.WriteFile(filepath.Join(dir, "main.go"), []byte(content), 0644)

	skill := &Skill{
		Name:     "test_skill",
		Language: "go",
		Dir:      dir,
		Hash:     knownHash(content), // matches what's on disk
	}

	got := ResolveTrust(skill)
	if got != TrustSecondParty {
		t.Errorf("ResolveTrust() = %s, want %s", got, TrustSecondParty)
	}
}

func TestResolveTrustThirdParty(t *testing.T) {
	dir := t.TempDir()
	content := "package main\n\nfunc main() {}\n"
	os.WriteFile(filepath.Join(dir, "main.go"), []byte(content), 0644)

	skill := &Skill{
		Name:     "test_skill",
		Language: "go",
		Dir:      dir,
		Hash:     "sha256:0000000000000000000000000000000000000000000000000000000000000000",
	}

	got := ResolveTrust(skill)
	if got != TrustThirdParty {
		t.Errorf("ResolveTrust() = %s, want %s", got, TrustThirdParty)
	}
}

func TestResolveTrustFourthPartyNoHash(t *testing.T) {
	skill := &Skill{
		Name:     "test_skill",
		Language: "go",
		Dir:      t.TempDir(),
		Hash:     "", // no hash → 4th party
	}

	got := ResolveTrust(skill)
	if got != TrustFourthParty {
		t.Errorf("ResolveTrust() = %s, want %s", got, TrustFourthParty)
	}
}

func TestResolveTrustFourthPartyMissingSource(t *testing.T) {
	// Hash is set but source file doesn't exist → can't verify → 4th party.
	skill := &Skill{
		Name:     "test_skill",
		Language: "go",
		Dir:      t.TempDir(),
		Hash:     "sha256:deadbeef",
	}

	got := ResolveTrust(skill)
	if got != TrustFourthParty {
		t.Errorf("ResolveTrust() = %s, want %s (missing source should fail closed)", got, TrustFourthParty)
	}
}

// ---------------------------------------------------------------------------
// EffectiveTimeout
// ---------------------------------------------------------------------------

func TestEffectiveTimeout(t *testing.T) {
	tests := []struct {
		name       string
		declared   string
		trustLevel TrustLevel
		want       time.Duration
	}{
		{
			name:       "2nd-party within cap",
			declared:   "15s",
			trustLevel: TrustSecondParty,
			want:       15 * time.Second,
		},
		{
			name:       "2nd-party at cap",
			declared:   "30s",
			trustLevel: TrustSecondParty,
			want:       30 * time.Second,
		},
		{
			name:       "3rd-party capped",
			declared:   "30s",
			trustLevel: TrustThirdParty,
			want:       10 * time.Second,
		},
		{
			name:       "4th-party capped",
			declared:   "15s",
			trustLevel: TrustFourthParty,
			want:       5 * time.Second,
		},
		{
			name:       "4th-party within cap",
			declared:   "3s",
			trustLevel: TrustFourthParty,
			want:       3 * time.Second,
		},
		{
			name:       "empty declared uses default, capped by tier",
			declared:   "",
			trustLevel: TrustFourthParty,
			want:       5 * time.Second, // default is 30s, capped to 5s
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			skill := &Skill{
				Permissions: Permissions{Timeout: tt.declared},
				TrustLevel:  tt.trustLevel,
			}
			got := EffectiveTimeout(skill)
			if got != tt.want {
				t.Errorf("EffectiveTimeout() = %v, want %v", got, tt.want)
			}
		})
	}
}
