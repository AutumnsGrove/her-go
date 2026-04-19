package config

// Tests for SetLocation — the surgical YAML edit that persists the user's
// home coordinates to config.yaml without trashing comments or formatting.
//
// We test the file-level behavior (byte-for-byte output checks), not just
// the in-memory struct, because the whole point of SetLocation is to
// preserve what's around it. A yaml.Marshal round-trip would pass any
// field check but strip every comment in the file.
//
// Table-driven tests are idiomatic Go for this shape of problem: a list
// of {input, expected-output} cases run through the same assertion code.
// Similar to pytest.mark.parametrize in Python, but built into testing.T.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTempConfig writes content to a fresh file inside t.TempDir() and
// returns its path. t.TempDir() gives us an isolated directory that gets
// auto-cleaned when the test ends — no leftover /tmp junk. Same idea as
// Python's tmp_path pytest fixture.
func writeTempConfig(t *testing.T, content string) string {
	t.Helper() // marks this as a test helper so failures report the caller's line, not ours
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0644); err != nil {
		t.Fatalf("writing temp config: %v", err)
	}
	return path
}

// readFile is a tiny convenience — SetLocation is fully about the file
// contents so every test reads the result and greps it.
func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading file: %v", err)
	}
	return string(data)
}

// TestSetLocation exercises the main shapes: update, append, partial
// section, preserved comments. Each subtest is named so a failure in
// `go test -v` shows exactly which case broke.
func TestSetLocation(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		lat      float64
		lon      float64
		locName  string
		// wantContains: substrings that MUST appear in the output.
		// Plain substring checks are easier to read than regex and
		// good enough for this surgical-edit test.
		wantContains []string
		// wantAbsent: substrings that must NOT appear (e.g., the old
		// placeholder value after an update).
		wantAbsent []string
	}{
		{
			name: "updates existing section in place",
			input: `identity:
  her: "Mira"

location:
  latitude: 0
  longitude: 0
  name: ""
  temp_unit: "fahrenheit"
  wind_unit: "mph"

voice:
  enabled: false
`,
			lat: 45.5152, lon: -122.6784, locName: "Portland, Oregon",
			wantContains: []string{
				"latitude: 45.5152",
				"longitude: -122.6784",
				`name: "Portland, Oregon"`,
				// Unit lines should survive untouched.
				`temp_unit: "fahrenheit"`,
				`wind_unit: "mph"`,
				// Surrounding sections should survive.
				`her: "Mira"`,
				"voice:",
				"enabled: false",
			},
			wantAbsent: []string{
				"latitude: 0\n",
				"longitude: 0\n",
				`name: ""`,
			},
		},
		{
			name: "preserves inline comments on existing lines",
			input: `location:
  latitude: 0                   # written by set_location
  longitude: 0
  name: ""
`,
			lat: 40.7128, lon: -74.006, locName: "New York, NY",
			wantContains: []string{
				"latitude: 40.7128",
				// The comment must survive the value swap.
				"# written by set_location",
				"longitude: -74.006",
				`name: "New York, NY"`,
			},
		},
		{
			name: "appends a new section when none exists",
			input: `identity:
  her: "Mira"

voice:
  enabled: false
`,
			lat: 35.6762, lon: 139.6503, locName: "Tokyo, Japan",
			wantContains: []string{
				// Existing content kept as-is.
				`her: "Mira"`,
				"voice:",
				// New block appended at the end.
				"location:",
				"latitude: 35.6762",
				"longitude: 139.6503",
				`name: "Tokyo, Japan"`,
			},
		},
		{
			name: "fills in missing fields when the section is partial",
			input: `location:
  latitude: 0
`,
			lat: 51.5074, lon: -0.1278, locName: "London, UK",
			wantContains: []string{
				"latitude: 51.5074",
				"longitude: -0.1278",
				`name: "London, UK"`,
			},
		},
		{
			name: "empty name preserves whatever was on disk",
			input: `location:
  latitude: 0
  longitude: 0
  name: "Keep Me"
`,
			lat: 1.23, lon: 4.56, locName: "",
			wantContains: []string{
				"latitude: 1.23",
				"longitude: 4.56",
				// With empty locName we don't touch the name line at all.
				`name: "Keep Me"`,
			},
		},
		{
			name: "formats negative and fractional coords without scientific notation",
			input: `location:
  latitude: 0
  longitude: 0
`,
			lat: -0.0001, lon: 179.987654321, locName: "Edge",
			wantContains: []string{
				// -0.0001 must round-trip cleanly (no scientific).
				"latitude: -0.0001",
				// 179.987654321 truncates to 6 decimals.
				"longitude: 179.987654",
			},
			wantAbsent: []string{"e-", "E-"}, // no scientific notation
		},
	}

	for _, tc := range tests {
		tc := tc // capture for the subtest closure — classic Go gotcha
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempConfig(t, tc.input)

			cfg := &Config{}
			if err := cfg.SetLocation(path, tc.lat, tc.lon, tc.locName); err != nil {
				t.Fatalf("SetLocation failed: %v", err)
			}

			got := readFile(t, path)

			for _, want := range tc.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("output missing %q\n--- full output ---\n%s", want, got)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("output should not contain %q\n--- full output ---\n%s", absent, got)
				}
			}

			// Also verify the in-memory struct was mutated. This is the
			// "subsequent tool calls in the same turn see new coords"
			// contract that set_location relies on.
			if cfg.Location.Latitude != tc.lat {
				t.Errorf("in-memory latitude = %v, want %v", cfg.Location.Latitude, tc.lat)
			}
			if cfg.Location.Longitude != tc.lon {
				t.Errorf("in-memory longitude = %v, want %v", cfg.Location.Longitude, tc.lon)
			}
			if tc.locName != "" && cfg.Location.Name != tc.locName {
				t.Errorf("in-memory name = %q, want %q", cfg.Location.Name, tc.locName)
			}
		})
	}
}

// TestSetLocation_IdempotentUpdate verifies that calling SetLocation
// twice with the same values doesn't corrupt the file — we shouldn't
// accumulate duplicate lines or drift formatting on re-writes. This is
// the scenario where the agent sets the same location repeatedly (e.g.,
// user confirms their city, and the model helpfully re-calls the tool).
func TestSetLocation_IdempotentUpdate(t *testing.T) {
	input := `location:
  latitude: 0
  longitude: 0
  name: ""
`
	path := writeTempConfig(t, input)
	cfg := &Config{}

	if err := cfg.SetLocation(path, 45.5, -122.6, "Portland"); err != nil {
		t.Fatalf("first call: %v", err)
	}
	after1 := readFile(t, path)

	if err := cfg.SetLocation(path, 45.5, -122.6, "Portland"); err != nil {
		t.Fatalf("second call: %v", err)
	}
	after2 := readFile(t, path)

	if after1 != after2 {
		t.Errorf("output changed between identical calls\n--- after1 ---\n%s\n--- after2 ---\n%s", after1, after2)
	}

	// Count latitude/longitude/name lines — we should have exactly one each,
	// not duplicates accumulating on each call.
	for _, key := range []string{"latitude:", "longitude:", "name:"} {
		count := strings.Count(after2, key)
		if count != 1 {
			t.Errorf("expected exactly 1 %q line, got %d\n%s", key, count, after2)
		}
	}
}

// TestSetLocation_MissingFile verifies that a broken path surfaces an
// error rather than silently creating a new file or panicking. The tool
// handler logs this and returns a user-facing warning, so the error
// return matters.
func TestSetLocation_MissingFile(t *testing.T) {
	cfg := &Config{}
	err := cfg.SetLocation("/nonexistent/path/config.yaml", 1.0, 2.0, "X")
	if err == nil {
		t.Fatal("expected error for missing config path, got nil")
	}
	if !strings.Contains(err.Error(), "reading config") {
		t.Errorf("error should mention reading config, got: %v", err)
	}
}

// TestFormatFloat covers the precision/notation corner cases formatFloat
// is specifically designed to avoid: trailing zeros (ugly in YAML) and
// scientific notation (would confuse humans reading config.yaml).
func TestFormatFloat(t *testing.T) {
	tests := []struct {
		in   float64
		want string
	}{
		{0, "0"},                      // zero: no trailing ".000000"
		{45.5, "45.5"},                // trailing zeros trimmed
		{45.500000, "45.5"},           // same even with explicit zeros
		{-122.6784, "-122.6784"},      // negative preserved
		{0.000001, "0.000001"},        // 6-decimal precision floor
		{179.987654321, "179.987654"}, // truncated, not rounded to scientific
		{-0.0001, "-0.0001"},          // tiny negative stays decimal
	}
	for _, tc := range tests {
		got := formatFloat(tc.in)
		if got != tc.want {
			t.Errorf("formatFloat(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
