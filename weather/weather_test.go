package weather

// Tests for the pure functions in weather.go — no network calls needed.
// wmoDescription and Format are the main targets: both have branching
// logic that's easy to cover with table-driven tests.

import (
	"strings"
	"testing"
	"time"
)

// TestWmoDescription covers every WMO code range in the switch statement.
// The codes are a worldwide standard, so these won't change — but the
// switch has range boundaries (51-57, 61-65, etc.) that are worth
// exercising at the edges.
func TestWmoDescription(t *testing.T) {
	tests := []struct {
		code int
		want string
	}{
		{0, "clear sky"},
		{1, "mainly clear"},
		{2, "partly cloudy"},
		{3, "overcast"},
		{45, "foggy"},
		{48, "foggy"},
		{51, "drizzle"},
		{55, "drizzle"},
		{57, "drizzle"},
		{61, "rain"},
		{63, "rain"},
		{65, "rain"},
		{66, "freezing rain"},
		{67, "freezing rain"},
		{71, "snow"},
		{75, "snow"},
		{77, "snow"},
		{80, "rain showers"},
		{82, "rain showers"},
		{85, "snow showers"},
		{86, "snow showers"},
		{95, "thunderstorm"},
		{99, "thunderstorm"},
		// Edge cases: codes that fall between defined ranges.
		{4, "unknown"},
		{50, "unknown"},
		{60, "unknown"},
		{70, "unknown"},
		{90, "unknown"},
	}

	for _, tc := range tests {
		got := wmoDescription(tc.code)
		if got != tc.want {
			t.Errorf("wmoDescription(%d) = %q, want %q", tc.code, got, tc.want)
		}
	}
}

// TestFormat exercises the Format function with various Current structs.
// Format is pure (no I/O), so we just check the output string shape.
func TestFormat(t *testing.T) {
	tests := []struct {
		name string
		in   *Current
		want []string // substrings that must appear
	}{
		{
			name: "typical sunny day",
			in: &Current{
				Temperature: 72,
				TempUnit:    "°F",
				WeatherCode: 1,
				Description: "mainly clear",
				Precip:      0,
				WindSpeed:   8,
				WindUnit:    "mph",
				FetchedAt:   time.Now(),
			},
			want: []string{"72°F", "mainly clear", "no precipitation", "wind 8 mph"},
		},
		{
			name: "rainy with precipitation",
			in: &Current{
				Temperature: 48.5,
				TempUnit:    "°F",
				WeatherCode: 61,
				Description: "rain",
				Precip:      3.2,
				WindSpeed:   15,
				WindUnit:    "mph",
				FetchedAt:   time.Now(),
			},
			want: []string{"48°F", "rain", "3.2 mm precipitation", "wind 15 mph"},
		},
		{
			name: "celsius and km/h",
			in: &Current{
				Temperature: 22,
				TempUnit:    "°C",
				WeatherCode: 2,
				Description: "partly cloudy",
				Precip:      0,
				WindSpeed:   20,
				WindUnit:    "km/h",
				FetchedAt:   time.Now(),
			},
			want: []string{"22°C", "partly cloudy", "no precipitation", "wind 20 km/h"},
		},
		{
			name: "nil returns empty",
			in:   nil,
			want: []string{""},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := Format(tc.in)
			for _, sub := range tc.want {
				if !strings.Contains(got, sub) {
					t.Errorf("Format() missing %q\ngot: %s", sub, got)
				}
			}
		})
	}
}

// TestFormat_NilSafety ensures Format doesn't panic on nil input.
// This matters because the get_weather handler could theoretically
// pass nil if Fetch returns an error that gets swallowed.
func TestFormat_NilSafety(t *testing.T) {
	got := Format(nil)
	if got != "" {
		t.Errorf("Format(nil) = %q, want empty string", got)
	}
}
