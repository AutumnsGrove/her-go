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

// negativeParallelism catches the "it's not X, it's Y" family —
// what tropes.fyi calls the single most identifiable AI writing tell.
//
// The key insight is that the SEPARATOR between the negation and the
// positive restatement is the mechanical tell. Real humans rarely write
// "isn't X; it's Y" with that precise pivot. Variants caught:
//
//   - "isn't X; it's Y"  /  "is not X, it's Y"  /  "wasn't X — it's Y"
//   - "that's not X, that's Y"  /  "that's not X — it's Y"
//   - "not just X, it's/but Y"  /  "not merely X, it's/but Y"
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
		// "not just/merely X, it's/but Y" — requires the pivot separator,
		// so bare "not just" in normal English ("it's not just you") passes.
		`not (?:just|merely) .{1,40}[;,\x{2014}]\s*(?:it's|it is|that's|that is|but)` +
		`|` +
		// "less like X, more like Y"
		`less like .{1,40},\s*more like` +
		`|` +
		// "not because X, but because Y"
		`not because .{1,40},?\s*but because` +
		`)`,
)

const negParHint = "drop the 'it's not X, it's Y' reframe — say what it is directly"

// StyleResult holds the outcome of a deterministic style check.
// Pattern identifies which check matched (for logging and traces).
// Empty Pattern means no issue was found.
type StyleResult struct {
	Pattern string // machine-readable: "neg_parallelism", "em_dashes", etc.
	Hint    string // human-readable retry instruction for the chat model
}

// Matched returns true if a style issue was detected.
func (r StyleResult) Matched() bool { return r.Pattern != "" }

// hasStyleIssue checks the reply for common AI writing tics that are
// mechanically detectable. Returns a StyleResult with the first matching
// pattern and a short, actionable hint. Only the first match fires —
// one problem at a time keeps the retry focused.
func hasStyleIssue(text string) StyleResult {
	// Normalize curly/smart quotes to straight ASCII before matching.
	// Many LLMs output U+2018/U+2019 instead of U+0027,
	// which silently breaks every contraction in our regex patterns
	// ("that's", "isn't", "it's"). One replace fixes all branches.
	text = strings.ReplaceAll(text, "’", "'")
	text = strings.ReplaceAll(text, "‘", "'")
	lower := strings.ToLower(text)

	// --- Negative parallelism (the big one) ---

	// Structural check: requires the full "negation + separator + pivot"
	// shape. Bare "not just" without a pivot is normal English.
	if negativeParallelism.MatchString(text) {
		return StyleResult{Pattern: "neg_parallelism", Hint: negParHint}
	}

	// --- Other mechanical tics ---

	// Em dash overuse. prompt.md bans them outright but models can't resist.
	// Allow two (common in natural multi-sentence replies); flag three+.
	if strings.Count(text, "—") >= 3 {
		return StyleResult{Pattern: "em_dashes", Hint: "too many em dashes — rephrase without them"}
	}

	// "I love that" as a hollow affirmation. Fine buried in a sentence
	// ("I love that you tried"), but as a standalone opener it's a tic.
	if strings.HasPrefix(lower, "i love that") ||
		strings.Contains(lower, "\ni love that") {
		return StyleResult{Pattern: "i_love_that", Hint: "drop 'I love that' as an opener — react to the specific detail instead"}
	}

	// "Here's the thing" family — manufactured reveal / fake drama.
	for _, phrase := range []string{
		"here's the thing",
		"here's where it gets",
		"here's the kicker",
		"here's what most people",
	} {
		if strings.Contains(lower, phrase) {
			return StyleResult{Pattern: "heres_the_thing", Hint: "drop '" + phrase + "' — just say it directly"}
		}
	}

	// "It's worth noting" / "It bears mentioning" — filler transitions
	// that add nothing. Already banned in prompt.md but models ignore it.
	if strings.Contains(lower, "it's worth noting") ||
		strings.Contains(lower, "it bears mentioning") {
		return StyleResult{Pattern: "filler_transition", Hint: "drop the filler transition — just state the point"}
	}

	return StyleResult{}
}
