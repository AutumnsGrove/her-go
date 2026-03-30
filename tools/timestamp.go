// Package tools — timestamp stripping for fact text.
//
// Every saved fact already carries a system-level created_at timestamp,
// so embedding dates in the fact text ("User visited the park on March 29")
// is redundant and causes retrieval confusion. This file provides a regex-based
// preprocessing step that silently strips temporal references before save —
// no LLM call needed, no agent retry required.
package tools

import (
	"regexp"
	"strings"
	"unicode"
)

// StripTimestamps removes temporal references from a fact string before it
// is written to the database.
//
// What gets stripped:
//   - Specific dates: "on March 29", "on 2026-03-29", "on 03/29/2026"
//   - Relative time words: "today", "yesterday", "last Tuesday", "this morning"
//   - "As of <date>, " prefixes that models often prepend
//
// What is NOT stripped (intentional — these are durable patterns, not events):
//   - Recurring schedules: "on Thursdays", "every Monday"
//   - Duration phrases: "for 3 months", "since January"
//
// After stripping, the result is cleaned up: double spaces collapsed, leading
// commas trimmed, and the first letter re-capitalized if the removal left a
// lowercase start.
func StripTimestamps(fact string) string {
	result := fact

	// --- Specific date patterns ---

	// "As of 2026-03-29, " or "As of March 29, " — model-generated prefixes.
	result = reAsOfISO.ReplaceAllString(result, "")
	result = reAsOfNamed.ReplaceAllString(result, "")

	// ISO date: "on 2026-03-29" or bare "2026-03-29".
	result = reISODate.ReplaceAllString(result, "")

	// Numeric date: "on 03/29/2026" or bare "03/29/2026".
	result = reNumericDate.ReplaceAllString(result, "")

	// Named month + day + optional year: "on March 29" or "on March 29, 2026".
	// Day must be a number (\d{1,2}), so "on Thursdays" is NOT matched.
	result = reNamedDate.ReplaceAllString(result, "")

	// --- Relative time words ---

	// Single-word relatives: "today", "yesterday", "tonight".
	// \b is a word-boundary anchor — same concept as Python's re module.
	result = reSingleRelative.ReplaceAllString(result, "")

	// "this morning / afternoon / evening / week / month / year"
	result = reThisPhrase.ReplaceAllString(result, "")

	// "earlier today"
	result = reEarlierToday.ReplaceAllString(result, "")

	// "last Tuesday / last week / last month" — but NOT "last name".
	// Only strip when followed by a day-of-week, "week", or "month".
	result = reLastRelative.ReplaceAllString(result, "")

	// "next Tuesday / next week / next month"
	result = reNextRelative.ReplaceAllString(result, "")

	// --- Cleanup ---

	// Collapse whitespace — strings.Fields splits on any whitespace,
	// Join re-assembles with single spaces. Same as Python's " ".join(s.split()).
	cleaned := strings.Join(strings.Fields(result), " ")
	cleaned = strings.TrimSpace(cleaned)

	// Strip a leading comma left behind when the removed phrase was at
	// the start: ", went to the gym" → "went to the gym".
	cleaned = strings.TrimLeft(cleaned, ", ")
	cleaned = strings.TrimSpace(cleaned)

	if cleaned == fact {
		return fact
	}

	// Re-capitalize the first letter. Go strings are immutable byte slices,
	// so we convert to runes for safe multi-byte character handling.
	if len(cleaned) > 0 {
		runes := []rune(cleaned)
		runes[0] = unicode.ToUpper(runes[0])
		cleaned = string(runes)
	}

	factLog.Info("stripped timestamp from fact", "before", fact, "after", cleaned)
	return cleaned
}

// --- Compiled regexes ---
//
// Compiling at package level (not inside the function) is a Go idiom.
// regexp.MustCompile panics if the pattern is invalid — that's fine for
// compile-time constants, same as Python's re.compile() at module level.
//
// Go uses RE2 (no lookaheads/lookbehinds), so we handle edge cases like
// "on Thursdays" by being specific about what we match (day = \d{1,2}).

var (
	reAsOfISO      = regexp.MustCompile(`(?i)\bAs of \d{4}-\d{2}-\d{2},?\s*`)
	reAsOfNamed    = regexp.MustCompile(`(?i)\bAs of ` + namedMonthDay + `,?\s*`)
	reISODate      = regexp.MustCompile(`(?i)\bon\s+\d{4}-\d{2}-\d{2}|\b\d{4}-\d{2}-\d{2}\b`)
	reNumericDate  = regexp.MustCompile(`(?i)\bon\s+\d{1,2}/\d{1,2}/\d{2,4}|\b\d{1,2}/\d{1,2}/\d{2,4}\b`)
	reNamedDate    = regexp.MustCompile(`(?i)\bon\s+` + namedMonthDay + `(,\s*\d{4})?`)
	reSingleRelative = regexp.MustCompile(`(?i)\b(today|yesterday|tonight)\b,?\s*`)
	reThisPhrase   = regexp.MustCompile(`(?i)\bthis\s+(morning|afternoon|evening|week|month|year)\b,?\s*`)
	reEarlierToday = regexp.MustCompile(`(?i)\bearlier today\b,?\s*`)
	reLastRelative = regexp.MustCompile(`(?i)\blast\s+(monday|tuesday|wednesday|thursday|friday|saturday|sunday|week|month)\b,?\s*`)
	reNextRelative = regexp.MustCompile(`(?i)\bnext\s+(monday|tuesday|wednesday|thursday|friday|saturday|sunday|week|month)\b,?\s*`)
)

// namedMonthDay is a regex fragment matching a named month followed by a
// numeric day. Extracted as a constant so it can be reused across patterns.
const namedMonthDay = `(January|February|March|April|May|June|July|August|September|October|November|December)\s+\d{1,2}`
