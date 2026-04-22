package tools

import (
	"testing"
)

// TestParseShiftNotes covers the main parsing shapes: full metadata, partial,
// freeform only, and empty notes. Table-driven like the rest of the test suite.
func TestParseShiftNotes(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantPosition string
		wantTrainer  string
		wantTimeChit string
		wantFreeform string
	}{
		{
			name:         "full shift metadata with freeform",
			input:        "position: Grill Cook\ntrainer: Mike\ntime chit: 6h 15m\nstayed late to close",
			wantPosition: "Grill Cook",
			wantTrainer:  "Mike",
			wantTimeChit: "6h 15m",
			wantFreeform: "stayed late to close",
		},
		{
			name:         "position only",
			input:        "position: Cashier",
			wantPosition: "Cashier",
			wantFreeform: "",
		},
		{
			name:         "time chit only",
			input:        "time chit: 8h 0m",
			wantTimeChit: "8h 0m",
		},
		{
			name:         "freeform only (no shift data)",
			input:        "Pick up milk on the way home",
			wantFreeform: "Pick up milk on the way home",
		},
		{
			name:  "empty notes",
			input: "",
		},
		{
			name:         "case insensitive keys",
			input:        "Position: Lead\nTRAINER: Sarah\nTime Chit: 4h 30m",
			wantPosition: "Lead",
			wantTrainer:  "Sarah",
			wantTimeChit: "4h 30m",
		},
		{
			name:         "extra whitespace around values",
			input:        "position:   Grill Cook  \ntrainer:  Mike  ",
			wantPosition: "Grill Cook",
			wantTrainer:  "Mike",
		},
		{
			name:         "mixed metadata and freeform lines",
			input:        "position: Grill Cook\nBoss said to come in early tomorrow\ntime chit: 6h 0m\nAlso grab aprons from storage",
			wantPosition: "Grill Cook",
			wantTimeChit: "6h 0m",
			wantFreeform: "Boss said to come in early tomorrow\nAlso grab aprons from storage",
		},
		{
			name:         "unknown key: value lines stay in freeform",
			input:        "position: Cashier\nfavorite color: blue\nreminder: bring lunch",
			wantPosition: "Cashier",
			wantFreeform: "favorite color: blue\nreminder: bring lunch",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ParseShiftNotes(tc.input)

			if got.Position != tc.wantPosition {
				t.Errorf("Position = %q, want %q", got.Position, tc.wantPosition)
			}
			if got.Trainer != tc.wantTrainer {
				t.Errorf("Trainer = %q, want %q", got.Trainer, tc.wantTrainer)
			}
			if got.TimeChit != tc.wantTimeChit {
				t.Errorf("TimeChit = %q, want %q", got.TimeChit, tc.wantTimeChit)
			}
			if got.Freeform != tc.wantFreeform {
				t.Errorf("Freeform = %q, want %q", got.Freeform, tc.wantFreeform)
			}
		})
	}
}

// TestSerializeShiftNotes verifies that serialization produces clean,
// human-readable output suitable for Apple Calendar's notes field.
func TestSerializeShiftNotes(t *testing.T) {
	tests := []struct {
		name  string
		input ShiftNotes
		want  string
	}{
		{
			name:  "full metadata with freeform",
			input: ShiftNotes{Position: "Grill Cook", Trainer: "Mike", TimeChit: "6h 15m", Freeform: "stayed late"},
			want:  "position: Grill Cook\ntrainer: Mike\ntime chit: 6h 15m\n\nstayed late",
		},
		{
			name:  "position only",
			input: ShiftNotes{Position: "Cashier"},
			want:  "position: Cashier",
		},
		{
			name:  "freeform only",
			input: ShiftNotes{Freeform: "just a regular note"},
			want:  "just a regular note",
		},
		{
			name:  "empty",
			input: ShiftNotes{},
			want:  "",
		},
		{
			name:  "metadata without freeform",
			input: ShiftNotes{Position: "Lead", TimeChit: "8h 0m"},
			want:  "position: Lead\ntime chit: 8h 0m",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := SerializeShiftNotes(tc.input)
			if got != tc.want {
				t.Errorf("got:\n%s\n\nwant:\n%s", got, tc.want)
			}
		})
	}
}

// TestParseSerializeRoundTrip verifies that parse → serialize → parse
// produces the same result. This is the key property for calendar_update:
// we parse existing notes, modify one field, serialize, and the rest must
// survive intact.
func TestParseSerializeRoundTrip(t *testing.T) {
	original := "position: Grill Cook\ntrainer: Mike\ntime chit: 6h 15m\n\nstayed late to close"

	sn := ParseShiftNotes(original)
	serialized := SerializeShiftNotes(sn)
	sn2 := ParseShiftNotes(serialized)

	if sn.Position != sn2.Position {
		t.Errorf("Position changed: %q → %q", sn.Position, sn2.Position)
	}
	if sn.Trainer != sn2.Trainer {
		t.Errorf("Trainer changed: %q → %q", sn.Trainer, sn2.Trainer)
	}
	if sn.TimeChit != sn2.TimeChit {
		t.Errorf("TimeChit changed: %q → %q", sn.TimeChit, sn2.TimeChit)
	}
	if sn.Freeform != sn2.Freeform {
		t.Errorf("Freeform changed: %q → %q", sn.Freeform, sn2.Freeform)
	}
}

// TestMergeShiftNotes verifies the merge operation used by calendar_update.
// Non-empty fields overwrite, empty fields leave existing values alone.
func TestMergeShiftNotes(t *testing.T) {
	existing := "position: Grill Cook\ntrainer: Mike\n\noriginal note"

	// Update only time chit — position and trainer should survive
	result := MergeShiftNotes(existing, "", "", "6h 15m", "")
	sn := ParseShiftNotes(result)

	if sn.Position != "Grill Cook" {
		t.Errorf("Position should be preserved, got %q", sn.Position)
	}
	if sn.Trainer != "Mike" {
		t.Errorf("Trainer should be preserved, got %q", sn.Trainer)
	}
	if sn.TimeChit != "6h 15m" {
		t.Errorf("TimeChit should be updated, got %q", sn.TimeChit)
	}
	if sn.Freeform != "original note" {
		t.Errorf("Freeform should be preserved, got %q", sn.Freeform)
	}
}

// TestMergeShiftNotes_ReplaceFreeform verifies that passing newFreeform
// replaces the existing freeform text entirely.
func TestMergeShiftNotes_ReplaceFreeform(t *testing.T) {
	existing := "position: Cashier\n\nold note"

	result := MergeShiftNotes(existing, "", "", "", "new note replaces old")
	sn := ParseShiftNotes(result)

	if sn.Position != "Cashier" {
		t.Errorf("Position should be preserved, got %q", sn.Position)
	}
	if sn.Freeform != "new note replaces old" {
		t.Errorf("Freeform should be replaced, got %q", sn.Freeform)
	}
}

// TestBuildShiftNotes verifies the convenience builder for calendar_create.
func TestBuildShiftNotes(t *testing.T) {
	got := BuildShiftNotes("Grill Cook", "Mike", "first day training")
	want := "position: Grill Cook\ntrainer: Mike\n\nfirst day training"
	if got != want {
		t.Errorf("got:\n%s\n\nwant:\n%s", got, want)
	}
}
