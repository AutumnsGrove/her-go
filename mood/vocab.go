// Package mood implements the Apple-style state-of-mind tracker.
//
// See docs/plans/PLAN-mood-tracking-redesign.md for the full design.
// This file owns vocabulary loading: valence buckets, feeling labels
// grouped by valence tier, and life-area associations. Everything else
// in this package reads from a *Vocab — the vocab file (mood/vocab.yaml
// by default) is the single source of truth.
package mood

import (
	_ "embed"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// Tier names a broad valence category used to filter feeling labels
// in the /mood wizard. When the user picks a valence bucket, the
// wizard shows only labels from that tier — no scrolling through 35
// choices to find "Amused" when you felt a 2/7.
type Tier string

const (
	TierUnpleasant Tier = "unpleasant" // buckets 1-3
	TierNeutral    Tier = "neutral"    // bucket 4
	TierPleasant   Tier = "pleasant"   // buckets 5-7
)

// ValenceBucket is one of the 7 mood "slots" the user picks from.
// Numbering: 1 = Very Unpleasant, 7 = Very Pleasant.
type ValenceBucket struct {
	// Value is the canonical 1-7 integer that lands in mood_entries.valence.
	Value int `yaml:"-"`

	// Label is the human-readable name shown in chart legends and prose.
	Label string `yaml:"label"`

	// Emoji is what appears on the wizard button for this bucket.
	Emoji string `yaml:"emoji"`

	// Color is a hex RGB string (e.g. "#F6A945") used by the PNG graph
	// renderer to shade this bucket's time band on the valence chart.
	Color string `yaml:"color"`
}

// Vocab is the fully-parsed mood vocabulary. Immutable once loaded;
// all maps/slices are returned as internal pointers so callers MUST
// treat them read-only. (Hot reload, when it lands, swaps the whole
// *Vocab atomically rather than mutating in place.)
type Vocab struct {
	// Buckets keyed 1..7 → ValenceBucket.
	Buckets map[int]ValenceBucket

	// labelsByTier keeps the original authoring order so the wizard
	// shows chips in a predictable, alphabetical layout.
	labelsByTier map[Tier][]string

	// labelTiers is the reverse map — label → tier — so the mood
	// agent's validator can ask "does this label match the chosen
	// valence bucket's tier?" in O(1).
	labelTiers map[string]Tier

	// associationsList is the flat ordered list of life-area
	// associations (Work, Family, …).
	associationsList []string

	// associationSet gives O(1) "is this a known association" checks
	// for agent output validation.
	associationSet map[string]struct{}
}

// rawVocab mirrors the on-disk YAML exactly — it's the structural
// input to yaml.Unmarshal. Post-parse we convert it to the richer
// Vocab with fast-lookup maps.
type rawVocab struct {
	ValenceBuckets map[int]ValenceBucket `yaml:"valence_buckets"`
	Labels         struct {
		Unpleasant []string `yaml:"unpleasant"`
		Neutral    []string `yaml:"neutral"`
		Pleasant   []string `yaml:"pleasant"`
	} `yaml:"labels"`
	Associations []string `yaml:"associations"`
}

//go:embed vocab.yaml
var defaultVocabYAML []byte

// Default returns the vocabulary compiled into the binary (mood/vocab.yaml
// as of build time). Useful for tests and for the bot's first boot
// before a user vocab file exists. Panics if the embedded YAML is
// somehow invalid — that would be a build-time bug worth crashing on.
func Default() *Vocab {
	v, err := parseVocab(defaultVocabYAML, "<embedded>")
	if err != nil {
		panic(fmt.Sprintf("mood: embedded vocab is invalid: %v", err))
	}
	return v
}

// LoadVocab reads a vocab YAML file from disk and parses it.
// Returns a validated *Vocab or a descriptive error. The error message
// includes the file path so a misconfigured task isn't diagnosed from
// a bare parser trace.
func LoadVocab(path string) (*Vocab, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("mood: reading %s: %w", path, err)
	}
	return parseVocab(raw, path)
}

// parseVocab is the shared YAML → *Vocab path used by both LoadVocab
// and Default. Validation runs here, close to the bytes, so tests can
// assert on specific error messages.
func parseVocab(data []byte, source string) (*Vocab, error) {
	var rv rawVocab
	if err := yaml.Unmarshal(data, &rv); err != nil {
		return nil, fmt.Errorf("mood: parsing %s: %w", source, err)
	}

	// Validate valence buckets: must have exactly 1..7 populated.
	if len(rv.ValenceBuckets) != 7 {
		return nil, fmt.Errorf("mood: %s defines %d valence buckets, want 7", source, len(rv.ValenceBuckets))
	}
	for i := 1; i <= 7; i++ {
		b, ok := rv.ValenceBuckets[i]
		if !ok {
			return nil, fmt.Errorf("mood: %s missing valence bucket %d", source, i)
		}
		if strings.TrimSpace(b.Label) == "" {
			return nil, fmt.Errorf("mood: %s bucket %d has empty label", source, i)
		}
		if strings.TrimSpace(b.Emoji) == "" {
			return nil, fmt.Errorf("mood: %s bucket %d has empty emoji", source, i)
		}
		if !isLikelyHexColor(b.Color) {
			return nil, fmt.Errorf("mood: %s bucket %d color %q is not a #RRGGBB hex string", source, i, b.Color)
		}
	}

	// Assemble the rich Vocab.
	v := &Vocab{
		Buckets:          make(map[int]ValenceBucket, 7),
		labelsByTier:     make(map[Tier][]string, 3),
		labelTiers:       map[string]Tier{},
		associationsList: nil,
		associationSet:   map[string]struct{}{},
	}
	for i := 1; i <= 7; i++ {
		b := rv.ValenceBuckets[i]
		b.Value = i
		v.Buckets[i] = b
	}

	// Labels — normalize, de-duplicate, sort within each tier so the
	// wizard is stable regardless of YAML ordering.
	addLabels := func(tier Tier, in []string) error {
		seen := map[string]struct{}{}
		out := make([]string, 0, len(in))
		for _, l := range in {
			l = strings.TrimSpace(l)
			if l == "" {
				return fmt.Errorf("mood: %s %s labels contain an empty entry", source, tier)
			}
			if _, dup := seen[strings.ToLower(l)]; dup {
				return fmt.Errorf("mood: %s duplicate label %q in %s tier", source, l, tier)
			}
			if existing, hasCross := v.labelTiers[l]; hasCross {
				return fmt.Errorf("mood: %s label %q appears in both %s and %s tiers", source, l, existing, tier)
			}
			seen[strings.ToLower(l)] = struct{}{}
			out = append(out, l)
			v.labelTiers[l] = tier
		}
		sort.Strings(out)
		v.labelsByTier[tier] = out
		return nil
	}

	if err := addLabels(TierUnpleasant, rv.Labels.Unpleasant); err != nil {
		return nil, err
	}
	if err := addLabels(TierNeutral, rv.Labels.Neutral); err != nil {
		return nil, err
	}
	if err := addLabels(TierPleasant, rv.Labels.Pleasant); err != nil {
		return nil, err
	}

	if len(rv.Labels.Unpleasant)+len(rv.Labels.Neutral)+len(rv.Labels.Pleasant) == 0 {
		return nil, fmt.Errorf("mood: %s has no labels defined", source)
	}

	// Associations — de-duplicate (case-insensitive), preserve
	// authoring order (tests on the wizard compare to this order).
	seenAssoc := map[string]struct{}{}
	for _, a := range rv.Associations {
		a = strings.TrimSpace(a)
		if a == "" {
			return nil, fmt.Errorf("mood: %s associations contain an empty entry", source)
		}
		key := strings.ToLower(a)
		if _, dup := seenAssoc[key]; dup {
			return nil, fmt.Errorf("mood: %s duplicate association %q", source, a)
		}
		seenAssoc[key] = struct{}{}
		v.associationsList = append(v.associationsList, a)
		v.associationSet[a] = struct{}{}
	}
	if len(v.associationsList) == 0 {
		return nil, fmt.Errorf("mood: %s has no associations defined", source)
	}

	return v, nil
}

// TierForValence returns the tier that contains the given valence
// value. The caller is responsible for passing 1..7; anything outside
// that range returns TierNeutral as a safe default plus false.
func (v *Vocab) TierForValence(valence int) (Tier, bool) {
	switch {
	case valence >= 1 && valence <= 3:
		return TierUnpleasant, true
	case valence == 4:
		return TierNeutral, true
	case valence >= 5 && valence <= 7:
		return TierPleasant, true
	default:
		return TierNeutral, false
	}
}

// LabelsForTier returns the feeling labels for the given tier, sorted
// alphabetically. Returns a copy — callers can't mutate the vocab.
func (v *Vocab) LabelsForTier(tier Tier) []string {
	src := v.labelsByTier[tier]
	out := make([]string, len(src))
	copy(out, src)
	return out
}

// LabelsForValence is a convenience wrapper: picks the tier for the
// given valence and returns its labels. Empty when valence is out of
// range.
func (v *Vocab) LabelsForValence(valence int) []string {
	tier, ok := v.TierForValence(valence)
	if !ok {
		return nil
	}
	return v.LabelsForTier(tier)
}

// IsLabel reports whether s is a registered feeling label (case-
// sensitive). The mood agent uses this to drop hallucinated labels
// before they reach the DB.
func (v *Vocab) IsLabel(s string) bool {
	_, ok := v.labelTiers[s]
	return ok
}

// IsAssociation reports whether s is a registered life-area association.
func (v *Vocab) IsAssociation(s string) bool {
	_, ok := v.associationSet[s]
	return ok
}

// Associations returns a copy of the authoring-ordered association list.
func (v *Vocab) Associations() []string {
	out := make([]string, len(v.associationsList))
	copy(out, v.associationsList)
	return out
}

// AllLabels returns every feeling label across all tiers, sorted
// alphabetically. Used by chart label axes.
func (v *Vocab) AllLabels() []string {
	all := make([]string, 0, len(v.labelTiers))
	for l := range v.labelTiers {
		all = append(all, l)
	}
	sort.Strings(all)
	return all
}

// isLikelyHexColor is a light validator for the #RRGGBB format. We
// don't need strict CSS parsing — we just want to catch typos like
// missing '#' or a 3-char short form, which would break the chart
// renderer.
func isLikelyHexColor(s string) bool {
	if len(s) != 7 || s[0] != '#' {
		return false
	}
	for i := 1; i < 7; i++ {
		c := s[i]
		isHex := (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
		if !isHex {
			return false
		}
	}
	return true
}
