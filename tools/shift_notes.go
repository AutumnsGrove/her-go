// Package tools provides the shift notes parser/serializer.
//
// Shift metadata lives in the notes field of calendar events as human-readable
// key: value pairs. This file handles parsing notes into structured data and
// serializing them back — the "codec" for shift metadata embedded in notes.
//
// The format is designed to look good in Apple Calendar while being reliably
// parseable in Go. Example:
//
//	position: Grill Cook
//	trainer: Mike
//	time chit: 6h 15m
//	stayed late to close, covered for Sarah
//
// Lines matching "key: value" are shift metadata. Everything else is freeform
// notes that appear at the bottom. This is similar to YAML front matter in
// markdown, but without delimiters — simpler for a calendar notes field.
package tools

import (
	"fmt"
	"regexp"
	"strings"
)

// ShiftNotes holds parsed shift metadata extracted from a calendar event's
// notes field. Fields are empty strings when not present — Go's zero value
// for strings, same idea as Python's None but without needing Optional[str].
type ShiftNotes struct {
	Position  string // role/position worked (e.g., "Grill Cook")
	Trainer   string // training supervisor (optional, temporary)
	TimeChit  string // actual hours worked (e.g., "6h 15m")
	Freeform  string // everything that isn't a key: value pair
}

// Known shift keys — the parser recognizes these (case-insensitive).
// Using a map for O(1) lookup instead of a list scan. The keys are
// lowercase for case-insensitive matching.
var shiftKeys = map[string]bool{
	"position":  true,
	"trainer":   true,
	"time chit": true,
}

// shiftKeyPattern matches lines like "position: Grill Cook" or "time chit: 6h 15m".
// The key can contain spaces (like "time chit") and the value is everything after
// the colon+space. This is more specific than a generic key: value regex because
// we only match KNOWN shift keys — random lines like "note: something" won't
// be treated as shift metadata.
var shiftKeyPattern = regexp.MustCompile(`(?i)^(position|trainer|time chit):\s+(.+)$`)

// ParseShiftNotes extracts shift metadata from a notes string. Lines that
// match known shift keys become structured fields; everything else goes
// into Freeform. Returns a ShiftNotes struct — all fields empty if the
// notes contain no shift metadata.
//
// This is the "read" half of the round-trip. The "write" half is
// SerializeShiftNotes, which reconstructs the notes string from the struct.
func ParseShiftNotes(notes string) ShiftNotes {
	var sn ShiftNotes
	var freeformLines []string

	for _, line := range strings.Split(notes, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			// Preserve blank lines in freeform (they're intentional spacing)
			freeformLines = append(freeformLines, "")
			continue
		}

		match := shiftKeyPattern.FindStringSubmatch(trimmed)
		if match != nil {
			// match[1] = key (mixed case from the original), match[2] = value
			key := strings.ToLower(match[1])
			value := strings.TrimSpace(match[2])

			switch key {
			case "position":
				sn.Position = value
			case "trainer":
				sn.Trainer = value
			case "time chit":
				sn.TimeChit = value
			}
		} else {
			freeformLines = append(freeformLines, trimmed)
		}
	}

	// Trim leading/trailing blank lines from freeform, then join.
	// This prevents extra whitespace when notes are just key: value pairs.
	sn.Freeform = strings.TrimSpace(strings.Join(freeformLines, "\n"))
	return sn
}

// SerializeShiftNotes reconstructs a notes string from structured shift data.
// Key: value pairs come first (in a consistent order), followed by freeform
// text. Only non-empty fields are included — no "position: " with blank values.
//
// The output is what the user sees in Apple Calendar, so readability matters.
func SerializeShiftNotes(sn ShiftNotes) string {
	var lines []string

	// Shift keys in display order — position first (most important),
	// then trainer, then time chit (added after the shift).
	if sn.Position != "" {
		lines = append(lines, fmt.Sprintf("position: %s", sn.Position))
	}
	if sn.Trainer != "" {
		lines = append(lines, fmt.Sprintf("trainer: %s", sn.Trainer))
	}
	if sn.TimeChit != "" {
		lines = append(lines, fmt.Sprintf("time chit: %s", sn.TimeChit))
	}

	if sn.Freeform != "" {
		// Blank line between metadata and freeform for readability
		if len(lines) > 0 {
			lines = append(lines, "")
		}
		lines = append(lines, sn.Freeform)
	}

	return strings.Join(lines, "\n")
}

// MergeShiftNotes parses existing notes, applies updates from the provided
// fields (non-empty strings overwrite, empty strings are left alone), and
// serializes back. This is the core operation for calendar_update when shift
// params are provided — it preserves existing metadata while updating only
// the fields the agent specified.
//
// If newFreeform is non-empty, it REPLACES the existing freeform text.
// Pass empty string to leave freeform unchanged.
func MergeShiftNotes(existingNotes, position, trainer, timeChit, newFreeform string) string {
	sn := ParseShiftNotes(existingNotes)

	if position != "" {
		sn.Position = position
	}
	if trainer != "" {
		sn.Trainer = trainer
	}
	if timeChit != "" {
		sn.TimeChit = timeChit
	}
	if newFreeform != "" {
		sn.Freeform = newFreeform
	}

	return SerializeShiftNotes(sn)
}

// BuildShiftNotes constructs notes for a new shift event from individual
// fields. Convenience wrapper for calendar_create — builds a ShiftNotes
// struct and serializes it in one call.
func BuildShiftNotes(position, trainer, existingNotes string) string {
	sn := ShiftNotes{
		Position: position,
		Trainer:  trainer,
		Freeform: existingNotes,
	}
	return SerializeShiftNotes(sn)
}
