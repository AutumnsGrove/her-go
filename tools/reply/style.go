// Package reply — deterministic style checks for AI writing tics.
//
// These run BEFORE the LLM classifier gate (if configured). They catch
// high-frequency mechanical patterns via string matching — free, fast,
// and deterministic. Only the FIRST match triggers a retry so we don't
// pile on and confuse the model.
//
// The philosophy: regex catches the predictable stuff, the classifier
// catches the nuanced stuff, and the prompt discourages everything else.
// All three layers fail-open — a reply always reaches the user.
package reply

import (
	"regexp"
	"strings"
)

// negativeParallelism catches the broader "it's not X, it's Y" family —
// what tropes.fyi calls the single most identifiable AI writing tell.
//
// The key insight is that the SEPARATOR between the negation and the
// positive restatement is the mechanical tell. Real humans rarely write
// "isn't X; it's Y" with that precise pivot. Variants caught:
//
//   - "isn't X; it's Y"  /  "is not X, it's Y"  /  "wasn't X — it's Y"
//   - "that's not X, that's Y"  /  "that's not X — it's Y"
//   - "less like X, more like Y"
//   - "not because X, but because Y"
var negativeParallelism = regexp.MustCompile(
	`(?i)(?:` +
		// "isn't X; it's Y" / "is not X, it's Y" / "wasn't X — it's Y"
		`(?:isn't|is not|wasn't|was not) .{1,40}[;,\x{2014}]\s*(?:it's|it is|that's|that is)` +
		`|` +
		// "that's not X, that's Y" / "that's not X — it's Y"
		`that's not .{1,40}[;,\x{2014}]\s*(?:it's|it is|that's|that is)` +
		`|` +
		// "less like X, more like Y"
		`less like .{1,40},\s*more like` +
		`|` +
		// "not because X, but because Y"
		`not because .{1,40},?\s*but because` +
		`)`,
)

const negParHint = "drop the 'it's not X, it's Y' reframe — say what it is directly"

// hasStyleIssue checks the reply for common AI writing tics that are
// mechanically detectable. Returns true and a short, actionable hint
// for the retry prompt if an issue is found. Only the first match
// fires — one problem at a time keeps the retry focused.
func hasStyleIssue(text string) (bool, string) {
	lower := strings.ToLower(text)

	// --- Negative parallelism (the big one) ---

	// Fast path: literal "not just" / "not merely" — the most common
	// variant, cheaper to catch with Contains than regex.
	if strings.Contains(lower, "not just") || strings.Contains(lower, "not merely") {
		return true, negParHint
	}

	// Broad structural check: catches "isn't X; it's Y", "less like X,
	// more like Y", and other variants the literal check misses.
	if negativeParallelism.MatchString(text) {
		return true, negParHint
	}

	// --- Other mechanical tics ---

	// Em dash overuse. prompt.md bans them outright but models can't resist.
	// Allow one (sometimes natural punctuation); flag two or more.
	if strings.Count(text, "\u2014") >= 2 {
		return true, "too many em dashes — rephrase without them"
	}

	// "I love that" as a hollow affirmation. Fine buried in a sentence
	// ("I love that you tried"), but as a standalone opener it's a tic.
	if strings.HasPrefix(lower, "i love that") ||
		strings.Contains(lower, "\ni love that") {
		return true, "drop 'I love that' as an opener — react to the specific detail instead"
	}

	// "Here's the thing" family — manufactured reveal / fake drama.
	for _, phrase := range []string{
		"here's the thing",
		"here's where it gets",
		"here's the kicker",
		"here's what most people",
	} {
		if strings.Contains(lower, phrase) {
			return true, "drop '" + phrase + "' — just say it directly"
		}
	}

	// "It's worth noting" / "It bears mentioning" — filler transitions
	// that add nothing. Already banned in prompt.md but models ignore it.
	if strings.Contains(lower, "it's worth noting") ||
		strings.Contains(lower, "it bears mentioning") {
		return true, "drop the filler transition — just state the point"
	}

	return false, ""
}
