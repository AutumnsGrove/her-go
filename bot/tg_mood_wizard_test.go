package bot

import "testing"

func TestParseWizardData(t *testing.T) {
	tests := []struct {
		data, action, arg string
	}{
		{"valence:3", "valence", "3"},
		{"label:Sad", "label", "Sad"},
		{"assoc:Work", "assoc", "Work"},
		{"next", "next", ""},
		{"cancel", "cancel", ""},
		{"save", "save", ""},
		{"", "", ""},
		{"label:With:Colons:Inside", "label", "With:Colons:Inside"},
	}
	for _, tc := range tests {
		action, arg := parseWizardData(tc.data)
		if action != tc.action || arg != tc.arg {
			t.Errorf("parseWizardData(%q) = (%q, %q), want (%q, %q)",
				tc.data, action, arg, tc.action, tc.arg)
		}
	}
}

func TestToggle(t *testing.T) {
	tests := []struct {
		in   []string
		add  string
		want []string
	}{
		{nil, "Sad", []string{"Sad"}},
		{[]string{"Sad"}, "Sad", []string{}},                       // remove
		{[]string{"Sad"}, "Stressed", []string{"Sad", "Stressed"}}, // add
		{[]string{"A", "B", "C"}, "B", []string{"A", "C"}},         // remove middle
	}
	for _, tc := range tests {
		got := toggle(tc.in, tc.add)
		if !slicesEqual(got, tc.want) {
			t.Errorf("toggle(%v, %q) = %v, want %v", tc.in, tc.add, got, tc.want)
		}
	}
}

// TestToggle_DoesNotMutateInput — the callers pass slice fields from
// shared state; aliasing a returned slice to the input backing array
// would create subtle bugs. Verify we always return a fresh slice.
func TestToggle_DoesNotMutateInput(t *testing.T) {
	in := []string{"A", "B"}
	out := toggle(in, "B")
	if len(out) != 1 {
		t.Fatalf("len(out) = %d, want 1", len(out))
	}
	if len(in) != 2 || in[0] != "A" || in[1] != "B" {
		t.Errorf("input slice was mutated: %v", in)
	}
}

func TestAtoiSafe(t *testing.T) {
	tests := []struct {
		s       string
		want    int
		wantErr bool
	}{
		{"0", 0, false},
		{"7", 7, false},
		{"42", 42, false},
		{"", 0, false}, // empty → 0, no error (matches the for loop's baseline)
		{"-1", 0, true},
		{"abc", 0, true},
		{"3.14", 0, true},
	}
	for _, tc := range tests {
		got, err := atoiSafe(tc.s)
		if (err != nil) != tc.wantErr {
			t.Errorf("atoiSafe(%q) err = %v, wantErr %v", tc.s, err, tc.wantErr)
		}
		if !tc.wantErr && got != tc.want {
			t.Errorf("atoiSafe(%q) = %d, want %d", tc.s, got, tc.want)
		}
	}
}

func slicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
