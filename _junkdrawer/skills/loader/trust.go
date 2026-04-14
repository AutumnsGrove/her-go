package loader

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// TrustLevel represents how much we trust a skill's source code.
//
// Trust is determined by SHA256 hash verification: the hash stored in
// skill.md is compared against the actual hash of the source file on disk.
// This is like a checksum — if someone (or the agent) modifies the source,
// the hash won't match and the skill gets demoted automatically.
//
// Lower number = higher trust. This makes comparisons intuitive:
//
//	if skill.TrustLevel <= TrustSecondParty { /* trusted */ }
//
// In Python terms, this is similar to an IntEnum — an int with named values.
// Go doesn't have enums, so we use typed constants with iota (auto-increment).
type TrustLevel int

const (
	// TrustFirstParty is for compiled-in tools (think, reply, save_fact, etc.)
	// Skills are never first-party — this exists for completeness in the model.
	TrustFirstParty TrustLevel = iota + 1 // 1

	// TrustSecondParty means the skill was written by Autumn and the hash
	// in skill.md matches the source on disk. Full permissions, no proxy.
	TrustSecondParty // 2

	// TrustThirdParty means the skill has a hash but it doesn't match —
	// the agent modified the source after Autumn vetted it. Gets proxied,
	// tighter timeout, read-only sidecar DB.
	TrustThirdParty // 3

	// TrustFourthParty means no hash exists in skill.md — the agent created
	// this skill from scratch. Maximum restrictions: proxied, shortest
	// timeout, no sidecar DB, no env vars.
	TrustFourthParty // 4
)

// String implements fmt.Stringer so TrustLevel prints nicely in logs
// and CLI output. Any type that has a String() method automatically gets
// used by fmt.Println, log.Info, etc. — like Python's __str__.
func (t TrustLevel) String() string {
	switch t {
	case TrustFirstParty:
		return "1st-party"
	case TrustSecondParty:
		return "2nd-party"
	case TrustThirdParty:
		return "3rd-party"
	case TrustFourthParty:
		return "4th-party"
	default:
		return fmt.Sprintf("unknown(%d)", int(t))
	}
}

// MaxTimeout returns the maximum allowed execution time for this trust tier.
// The skill's declared timeout is capped to this value.
//
//	2nd-party: 30s (full trust, same as default)
//	3rd-party: 10s (modified by agent, keep it short)
//	4th-party:  5s (untrusted, minimum viable)
func (t TrustLevel) MaxTimeout() time.Duration {
	switch t {
	case TrustSecondParty:
		return 30 * time.Second
	case TrustThirdParty:
		return 10 * time.Second
	case TrustFourthParty:
		return 5 * time.Second
	default:
		// First-party tools don't use this, but be safe.
		return 30 * time.Second
	}
}

// AllowDirectNetwork returns true if this trust tier allows direct outbound
// network access (no proxy). Currently only 2nd-party skills skip the proxy.
//
// This is a stub — the network proxy isn't built yet. When it is, the runner
// will check this to decide whether to set HTTP_PROXY env vars.
func (t TrustLevel) AllowDirectNetwork() bool {
	return t <= TrustSecondParty
}

// ---------------------------------------------------------------------------
// Hash computation
// ---------------------------------------------------------------------------

// ComputeSourceHash computes the SHA256 hash of a skill's main source file.
//
// For Go skills, this hashes main.go. For Python, main.py. The result is
// returned in "sha256:<hex>" format, matching the format stored in skill.md.
//
// We hash only the main source file, not all files in the directory. This
// keeps the model simple: one file, one hash. If the skill grows to multiple
// files, we can extend this later.
func ComputeSourceHash(skill *Skill) (string, error) {
	// Determine source file based on language.
	var filename string
	switch skill.Language {
	case "go":
		filename = "main.go"
	case "python":
		filename = "main.py"
	default:
		return "", fmt.Errorf("unsupported language for hashing: %s", skill.Language)
	}

	srcPath := filepath.Join(skill.Dir, filename)
	data, err := os.ReadFile(srcPath)
	if err != nil {
		return "", fmt.Errorf("reading source file: %w", err)
	}

	// crypto/sha256 is Go's stdlib for SHA-256. It works like Python's
	// hashlib.sha256() — feed it bytes, get a digest back.
	hash := sha256.Sum256(data)
	return "sha256:" + hex.EncodeToString(hash[:]), nil
}

// ---------------------------------------------------------------------------
// Trust resolution
// ---------------------------------------------------------------------------

// ResolveTrust determines a skill's trust level by comparing the hash stored
// in skill.md against the actual hash of the source file on disk.
//
// The logic is simple:
//   - No hash in skill.md         → 4th-party (agent-created, never vetted)
//   - Hash present, matches disk  → 2nd-party (vetted by Autumn)
//   - Hash present, doesn't match → 3rd-party (agent modified it)
//
// If we can't read the source file to compute the hash, we default to
// 4th-party (untrusted) — fail closed, not open.
func ResolveTrust(skill *Skill) TrustLevel {
	// No hash means the skill was never signed.
	if skill.Hash == "" {
		return TrustFourthParty
	}

	computed, err := ComputeSourceHash(skill)
	if err != nil {
		// Can't verify → untrusted. Log this so it's visible.
		log.Warn("trust: can't compute hash", "name", skill.Name, "error", err)
		return TrustFourthParty
	}

	if skill.Hash == computed {
		return TrustSecondParty
	}

	// Hash exists but doesn't match — someone changed the source.
	return TrustThirdParty
}

// ---------------------------------------------------------------------------
// Timeout enforcement
// ---------------------------------------------------------------------------

// EffectiveTimeout returns the actual timeout to use when running a skill.
// It takes the skill's declared timeout and caps it by the trust tier's max.
//
// For example, a 2nd-party skill declaring "timeout: 15s" gets 15s (within
// the 30s cap). A 4th-party skill declaring "timeout: 15s" gets 5s (capped).
func EffectiveTimeout(skill *Skill) time.Duration {
	declared := parseTimeout(skill.Permissions.Timeout)
	cap := skill.TrustLevel.MaxTimeout()

	if declared > cap {
		return cap
	}
	return declared
}
