// Package get_time — tests for the get_time tool handler.
//
// get_time is purely a read-only clock tool: it loads a timezone from config,
// formats time.Now() several ways, and returns a JSON object. Tests here
// validate the output shape, timezone routing, and the error path for an
// invalid timezone string in config.
package get_time

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"her/config"
	"her/tools"
)

// newCtx builds a minimal tools.Context with the given timezone string.
// An empty tz means "not configured" — the handler falls back to time.Local.
func newCtx(t *testing.T, tz string) *tools.Context {
	t.Helper()
	return &tools.Context{
		Cfg: &config.Config{
			Calendar: config.CalendarConfig{
				DefaultTimezone: tz,
			},
		},
	}
}

// parseResult unmarshals the JSON returned by Handle into a string map.
// It fatally fails the test if the result is not valid JSON, which means
// Handle returned an error string instead of a result object.
func parseResult(t *testing.T, result string) map[string]string {
	t.Helper()
	var out map[string]string
	if err := json.Unmarshal([]byte(result), &out); err != nil {
		t.Fatalf("Handle returned non-JSON output %q: %v", result, err)
	}
	return out
}

// TestHandle_AllKeysPresent verifies that the result JSON contains all four
// expected keys: iso, human, day_of_week, and timezone. If any key is missing,
// the agent model can't extract the time information it needs for scheduling.
func TestHandle_AllKeysPresent(t *testing.T) {
	ctx := newCtx(t, "UTC")

	result := Handle("", ctx)

	got := parseResult(t, result)

	requiredKeys := []string{"iso", "human", "day_of_week", "timezone"}
	for _, key := range requiredKeys {
		if _, ok := got[key]; !ok {
			t.Errorf("result JSON missing key %q; got keys: %v", key, keysOf(got))
		}
	}
}

// TestHandle_ValidTimezone verifies that a valid IANA timezone name in config
// is correctly reflected in the "timezone" field of the result. If this breaks,
// the agent will plan events in the wrong timezone even when config is correct.
func TestHandle_ValidTimezone(t *testing.T) {
	cases := []struct {
		name           string
		tz             string
		wantTZContains string
	}{
		{"UTC", "UTC", "UTC"},
		{"eastern", "America/New_York", "America/New_York"},
		{"pacific", "America/Los_Angeles", "America/Los_Angeles"},
		{"london", "Europe/London", "Europe/London"},
		{"tokyo", "Asia/Tokyo", "Asia/Tokyo"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := newCtx(t, tc.tz)

			result := Handle("{}", ctx)

			got := parseResult(t, result)

			if !strings.Contains(got["timezone"], tc.wantTZContains) {
				t.Errorf("timezone = %q, want it to contain %q", got["timezone"], tc.wantTZContains)
			}
		})
	}
}

// TestHandle_EmptyTimezoneUsesLocal verifies that when DefaultTimezone is
// empty (not configured), the handler falls back to time.Local without
// error. The result must still be valid JSON with all required keys.
// The "timezone" field will say "Local" on most Go runtime platforms.
func TestHandle_EmptyTimezoneUsesLocal(t *testing.T) {
	ctx := newCtx(t, "") // empty = not configured

	result := Handle("", ctx)

	// Must be valid JSON, not an error string.
	got := parseResult(t, result)

	// All keys must still be present.
	for _, key := range []string{"iso", "human", "day_of_week", "timezone"} {
		if _, ok := got[key]; !ok {
			t.Errorf("result missing key %q when timezone is empty", key)
		}
	}
}

// TestHandle_InvalidTimezone verifies that a garbage timezone string in config
// returns an error message (not a panic, not silently wrong output). The error
// string must contain the bad timezone name so the agent can relay it.
// This catches misconfigured config.yaml before silent misbehavior.
func TestHandle_InvalidTimezone(t *testing.T) {
	cases := []struct {
		name string
		tz   string
	}{
		{"gibberish", "Not/A/Timezone"},
		{"empty_slash", "/"},
		{"spaces", "America/ New York"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ctx := newCtx(t, tc.tz)

			result := Handle("", ctx)

			// Must start with "error:" — not valid JSON.
			if !strings.HasPrefix(result, "error:") {
				t.Errorf("Handle(%q) = %q, want error: prefix", tc.tz, result)
			}
			// Must name the bad timezone so the agent can report it.
			if !strings.Contains(result, tc.tz) {
				t.Errorf("error message %q does not contain the bad timezone %q", result, tc.tz)
			}
		})
	}
}

// TestHandle_ISOFormat verifies that the "iso" field is a valid RFC3339
// timestamp. The agent passes this to scheduling tools (calendar_create, etc.)
// which parse it as RFC3339 — a non-conforming format would cause silent
// failures downstream.
func TestHandle_ISOFormat(t *testing.T) {
	ctx := newCtx(t, "UTC")

	result := Handle("", ctx)
	got := parseResult(t, result)

	iso, ok := got["iso"]
	if !ok {
		t.Fatal("result JSON missing 'iso' key")
	}

	// time.Parse with RFC3339 is the same format the handler uses to write.
	// If parsing fails, the field is malformed.
	parsed, err := time.Parse(time.RFC3339, iso)
	if err != nil {
		t.Errorf("iso = %q is not valid RFC3339: %v", iso, err)
	}

	// Sanity-check: the parsed time should be very close to now.
	// We allow a 5-second window to account for test execution time.
	diff := time.Since(parsed)
	if diff < 0 {
		diff = -diff
	}
	if diff > 5*time.Second {
		t.Errorf("iso = %q is %v away from now; expected ≤5s", iso, diff)
	}
}

// TestHandle_HumanFieldContainsTimezoneAbbreviation verifies that the "human"
// field includes a timezone abbreviation (e.g. "UTC", "EST", "EDT") appended
// after the formatted time. Without it the agent cannot show the user an
// unambiguous human-readable time.
func TestHandle_HumanFieldContainsTimezoneAbbreviation(t *testing.T) {
	ctx := newCtx(t, "UTC")

	result := Handle("", ctx)
	got := parseResult(t, result)

	human, ok := got["human"]
	if !ok {
		t.Fatal("result JSON missing 'human' key")
	}

	// The handler appends `zone, _ := now.Zone()` — for UTC this is "UTC".
	// We just check the field is non-empty and contains a non-space suffix.
	if strings.TrimSpace(human) == "" {
		t.Errorf("human = %q, expected non-empty formatted time string", human)
	}

	// The format is "Monday, January 2, 2006 3:04 PM TZ" — must have ≥2 words.
	parts := strings.Fields(human)
	if len(parts) < 2 {
		t.Errorf("human = %q appears to be missing the timezone abbreviation suffix", human)
	}
}

// TestHandle_DayOfWeekIsValidDay verifies that "day_of_week" contains a full
// English weekday name (Monday through Sunday). A wrong value here would
// corrupt any agent reasoning about "is this a weekday?" type decisions.
func TestHandle_DayOfWeekIsValidDay(t *testing.T) {
	ctx := newCtx(t, "UTC")

	result := Handle("", ctx)
	got := parseResult(t, result)

	day, ok := got["day_of_week"]
	if !ok {
		t.Fatal("result JSON missing 'day_of_week' key")
	}

	validDays := map[string]bool{
		"Monday": true, "Tuesday": true, "Wednesday": true,
		"Thursday": true, "Friday": true, "Saturday": true, "Sunday": true,
	}
	if !validDays[day] {
		t.Errorf("day_of_week = %q, want a full English weekday name", day)
	}
}

// TestHandle_ArgsIgnored verifies that the args parameter is not parsed or
// required — get_time takes no parameters and must work regardless of what
// the model passes in. The agent prompt says args are optional, so the
// handler must be defensive.
func TestHandle_ArgsIgnored(t *testing.T) {
	ctx := newCtx(t, "UTC")

	cases := []struct {
		name string
		args string
	}{
		{"empty", ""},
		{"empty_object", "{}"},
		{"garbage", "not json at all"},
		{"unexpected_fields", `{"foo": "bar"}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result := Handle(tc.args, ctx)

			// All of these should produce valid JSON, not an error string.
			got := parseResult(t, result)
			if _, ok := got["iso"]; !ok {
				t.Errorf("Handle(%q) returned %q — expected valid result JSON", tc.args, result)
			}
		})
	}
}

// keysOf extracts the keys of a string map into a slice for readable error
// messages. Declared as a package-level helper rather than inline so it can
// be reused across tests without repetition.
func keysOf(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	return keys
}
