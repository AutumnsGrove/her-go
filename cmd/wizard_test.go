package cmd

import "testing"

// Table-driven tests for the three pure validator/helper functions in wizard.go.
// In Go, _test.go files are compiled only during `go test` — they can access
// unexported names in the same package (package cmd here, not package cmd_test).

func TestValidateOptionalInt64(t *testing.T) {
	cases := []struct {
		input   string
		wantErr bool
	}{
		{"", false},                    // blank = "not set yet" — always OK
		{"123456789", false},           // typical Telegram chat ID
		{"-1", false},                  // negative int64 is valid
		{"9223372036854775807", false}, // int64 max
		{"0", false},                   // zero is a valid int64
		{"abc", true},                  // not a number
		{"12.5", true},                 // float rejected
		{"9999999999999999999", true},  // overflows int64
	}

	for _, c := range cases {
		err := validateOptionalInt64(c.input)
		if (err != nil) != c.wantErr {
			t.Errorf("validateOptionalInt64(%q): got err=%v, wantErr=%v", c.input, err, c.wantErr)
		}
	}
}

func TestValidateOptionalPort(t *testing.T) {
	cases := []struct {
		input   string
		wantErr bool
	}{
		{"", false},      // blank = "not configured" — always OK
		{"1", false},     // min valid port
		{"8443", false},  // default webhook port
		{"65535", false}, // max valid port
		{"0", true},      // port 0 is not a user-facing port
		{"65536", true},  // one above max
		{"-1", true},     // negative
		{"abc", true},    // not a number
		{"80.5", true},   // float
	}

	for _, c := range cases {
		err := validateOptionalPort(c.input)
		if (err != nil) != c.wantErr {
			t.Errorf("validateOptionalPort(%q): got err=%v, wantErr=%v", c.input, err, c.wantErr)
		}
	}
}

func TestApplySensitive(t *testing.T) {
	cases := []struct {
		name     string
		original string
		newVal   string
		want     string
	}{
		{"empty new keeps original", "existing-key", "", "existing-key"},
		{"non-empty new replaces", "existing-key", "new-key", "new-key"},
		{"both empty stays empty", "", "", ""},
		{"empty original gets new", "", "fresh-key", "fresh-key"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := c.original
			applySensitive(&got, c.newVal)
			if got != c.want {
				t.Errorf("applySensitive(%q, %q) = %q, want %q", c.original, c.newVal, got, c.want)
			}
		})
	}
}
