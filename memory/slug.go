package memory

import (
	"fmt"
	"regexp"
	"strings"
)

// slugNonAlnum matches any run of characters that aren't lowercase letters
// or digits — everything else collapses into a single hyphen.
var slugNonAlnum = regexp.MustCompile(`[^a-z0-9]+`)

// maxSlugLen caps how long a derived slug can get. Long enough to stay
// readable ("donette-bit"), short enough that nobody's typing a paragraph
// into recall_memories(slug=...).
const maxSlugLen = 40

// Slugify converts arbitrary text into a lowercase, hyphenated identifier:
// "Donette bit!" -> "donette-bit". Used for turning a hook's short label
// into a slug — see DeriveSlug below for how that label gets extracted.
func Slugify(s string) string {
	s = strings.ToLower(s)
	s = slugNonAlnum.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > maxSlugLen {
		s = strings.TrimRight(s[:maxSlugLen], "-")
	}
	if s == "" {
		s = "memory"
	}
	return s
}

// DeriveSlug extracts a slug from a hook written in the project's
// "label — trigger" convention (see the two worked examples in
// docs/plans/PLAN-memory-hooks-dual-recall.md):
//
//	"Donette bit — call her this when a stranger uses a pet name on her"
//	"tends to over-hedge critical feedback — say it straight when asked for honesty"
//
// The label (before the em-dash) becomes the slug. If a hook doesn't follow
// the convention — no em-dash present — the first few words stand in
// instead, so every hook still gets a reasonable slug rather than an error.
func DeriveSlug(hook string) string {
	if label, _, found := strings.Cut(hook, "—"); found {
		return Slugify(label)
	}
	// No em-dash: fall back to the first ~5 words as the label.
	words := strings.Fields(hook)
	if len(words) > 5 {
		words = words[:5]
	}
	return Slugify(strings.Join(words, " "))
}

// UniqueSlug returns a slug derived from base that doesn't collide with an
// existing memory's slug. Appends -2, -3, ... until a free one is found.
// Returns base unchanged if it's already unique.
func (s *SQLiteStore) UniqueSlug(base string) (string, error) {
	slug := base
	for i := 2; ; i++ {
		existing, err := s.GetMemoryBySlug(slug)
		if err != nil {
			return "", fmt.Errorf("checking slug %q: %w", slug, err)
		}
		if existing == nil {
			return slug, nil
		}
		slug = fmt.Sprintf("%s-%d", base, i)
	}
}
