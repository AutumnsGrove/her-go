// Package integrate — tests for the Foursquare Places API formatting helpers.
//
// These tests cover the pure functions that turn Foursquare API responses into
// readable output. FormatPlaces produces a compact agent-facing summary;
// the user-facing place cards are tested in tools/context_test.go.
// No network calls — we build Place structs directly and verify the output.
package integrate

import (
	"strings"
	"testing"
)

// ---------------------------------------------------------------------------
// formatDistance — converts meters to human-friendly strings.
// Three regimes: short (meters), walkable (km + walk time), far (km only).
// ---------------------------------------------------------------------------

func TestFormatDistance(t *testing.T) {
	cases := []struct {
		name    string
		meters  int
		want    string // exact match
		contain string // substring match (when exact is empty)
	}{
		// Zero or negative → "nearby" (no distance info from Foursquare).
		{"zero", 0, "nearby", ""},
		{"negative", -1, "nearby", ""},

		// Under 1km → show raw meters.
		{"50m", 50, "50m away", ""},
		{"999m", 999, "999m away", ""},

		// 1km–2.4km → show km + walk time (~80m/min).
		{"1km", 1000, "", "1.0km (~13 min walk)"},
		{"2km", 2000, "", "2.0km (~25 min walk)"},
		{"2.4km_boundary", 2400, "", "2.4km (~30 min walk)"},

		// Beyond ~30 min walk → just show km, no walk time.
		{"2.5km_far", 2500, "", "2.5km away"},
		{"10km", 10000, "", "10.0km away"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatDistance(tc.meters)
			if tc.want != "" {
				if got != tc.want {
					t.Errorf("formatDistance(%d) = %q, want %q", tc.meters, got, tc.want)
				}
			} else if !strings.Contains(got, tc.contain) {
				t.Errorf("formatDistance(%d) = %q, want it to contain %q", tc.meters, got, tc.contain)
			}
		})
	}
}

// TestFormatDistance_Exported verifies the exported wrapper matches the
// internal function — nearby_search uses FormatDistance (exported) to
// build PlaceCards.
func TestFormatDistance_Exported(t *testing.T) {
	if FormatDistance(500) != formatDistance(500) {
		t.Error("FormatDistance and formatDistance should return the same value")
	}
}

// ---------------------------------------------------------------------------
// FormatPlaces — compact agent-facing summary (not user-facing).
// ---------------------------------------------------------------------------

func TestFormatPlaces_Empty(t *testing.T) {
	got := FormatPlaces(nil)
	if got != "No places found nearby." {
		t.Errorf("FormatPlaces(nil) = %q, want empty-state message", got)
	}

	got = FormatPlaces([]Place{})
	if got != "No places found nearby." {
		t.Errorf("FormatPlaces([]) = %q, want empty-state message", got)
	}
}

func TestFormatPlaces_SinglePlace(t *testing.T) {
	places := []Place{
		{
			Name:       "Blue Bottle Coffee",
			Distance:   350,
			Categories: []PlaceCategory{{Name: "Coffee Shop"}},
			Location: PlaceLocation{
				FormattedAddress: "123 Main St, Portland, OR",
			},
		},
	}

	got := FormatPlaces(places)

	// Agent summary should be compact — no bold, no Maps links.
	checks := []struct {
		label  string
		substr string
	}{
		{"numbering", "1. "},
		{"name", "Blue Bottle Coffee"},
		{"category", "(Coffee Shop)"},
		{"distance", "350m away"},
		{"address", "123 Main St, Portland, OR"},
	}

	for _, c := range checks {
		if !strings.Contains(got, c.substr) {
			t.Errorf("FormatPlaces missing %s: want %q in output:\n%s", c.label, c.substr, got)
		}
	}

	// Should NOT contain markdown bold — that's for the old user-facing format.
	if strings.Contains(got, "**") {
		t.Errorf("agent summary should not contain **bold** markdown:\n%s", got)
	}
}

func TestFormatPlaces_MultipleCategories(t *testing.T) {
	places := []Place{
		{
			Name: "Powell's",
			Categories: []PlaceCategory{
				{Name: "Bookstore"},
				{Name: "Gift Shop"},
			},
		},
	}

	got := FormatPlaces(places)

	if !strings.Contains(got, "(Bookstore, Gift Shop)") {
		t.Errorf("FormatPlaces should join categories with comma: got\n%s", got)
	}
}

func TestFormatPlaces_AddressFallback(t *testing.T) {
	// When FormattedAddress is empty, falls back to Address field.
	places := []Place{
		{
			Name: "Corner Store",
			Location: PlaceLocation{
				Address: "456 Oak Ave",
			},
		},
	}

	got := FormatPlaces(places)

	if !strings.Contains(got, "456 Oak Ave") {
		t.Errorf("FormatPlaces should fall back to Location.Address: got\n%s", got)
	}
}

func TestFormatPlaces_NoAddress(t *testing.T) {
	// When neither address field is set, no address should appear.
	places := []Place{
		{
			Name:     "Mystery Spot",
			Distance: 100,
		},
	}

	got := FormatPlaces(places)

	// Compact format: single line per place when no address.
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) != 1 {
		t.Errorf("FormatPlaces with no address should be 1 line, got %d:\n%s", len(lines), got)
	}
}

func TestFormatPlaces_Numbering(t *testing.T) {
	places := []Place{
		{Name: "First"},
		{Name: "Second"},
		{Name: "Third"},
	}

	got := FormatPlaces(places)

	if !strings.Contains(got, "1. First") {
		t.Errorf("missing '1. First' in:\n%s", got)
	}
	if !strings.Contains(got, "2. Second") {
		t.Errorf("missing '2. Second' in:\n%s", got)
	}
	if !strings.Contains(got, "3. Third") {
		t.Errorf("missing '3. Third' in:\n%s", got)
	}
}

// ---------------------------------------------------------------------------
// Helper functions — JoinCategories, PlaceAddress, PlaceMapsURL
// ---------------------------------------------------------------------------

func TestJoinCategories(t *testing.T) {
	cases := []struct {
		name string
		cats []PlaceCategory
		want string
	}{
		{"nil", nil, ""},
		{"empty", []PlaceCategory{}, ""},
		{"one", []PlaceCategory{{Name: "Cafe"}}, "Cafe"},
		{"two", []PlaceCategory{{Name: "Cafe"}, {Name: "Bakery"}}, "Cafe, Bakery"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := JoinCategories(tc.cats)
			if got != tc.want {
				t.Errorf("JoinCategories = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPlaceAddress(t *testing.T) {
	// Prefers FormattedAddress.
	p := Place{Location: PlaceLocation{
		FormattedAddress: "123 Main St",
		Address:          "123 Main",
	}}
	if got := PlaceAddress(p); got != "123 Main St" {
		t.Errorf("PlaceAddress should prefer FormattedAddress, got %q", got)
	}

	// Falls back to Address.
	p2 := Place{Location: PlaceLocation{Address: "456 Oak"}}
	if got := PlaceAddress(p2); got != "456 Oak" {
		t.Errorf("PlaceAddress fallback failed, got %q", got)
	}

	// Empty when neither set.
	p3 := Place{}
	if got := PlaceAddress(p3); got != "" {
		t.Errorf("PlaceAddress should be empty, got %q", got)
	}
}

func TestPlaceMapsURL(t *testing.T) {
	p := Place{Latitude: 45.523, Longitude: -122.676}

	got := PlaceMapsURL(p)
	if !strings.HasPrefix(got, "https://maps.google.com/?q=") {
		t.Errorf("PlaceMapsURL should start with maps URL, got %q", got)
	}
	if !strings.Contains(got, "45.523") {
		t.Errorf("PlaceMapsURL should contain latitude, got %q", got)
	}
}

func TestPlaceMapsURL_ZeroCoords(t *testing.T) {
	// Zero coords → empty URL (no geocode data available).
	p := Place{}
	if got := PlaceMapsURL(p); got != "" {
		t.Errorf("PlaceMapsURL with zero coords should be empty, got %q", got)
	}
}

// ---------------------------------------------------------------------------
// NewFoursquareClient — constructor nil-safety.
// ---------------------------------------------------------------------------

func TestNewFoursquareClient_EmptyKey(t *testing.T) {
	client := NewFoursquareClient("")
	if client != nil {
		t.Error("NewFoursquareClient(\"\") should return nil when API key is empty")
	}
}

func TestNewFoursquareClient_ValidKey(t *testing.T) {
	client := NewFoursquareClient("test-key-123")
	if client == nil {
		t.Fatal("NewFoursquareClient with valid key should not return nil")
	}
	if client.http == nil {
		t.Error("client.http should be initialized with timeout")
	}
}

// ---------------------------------------------------------------------------
// Constants — verify named constants and migration values.
// ---------------------------------------------------------------------------

func TestConstants(t *testing.T) {
	if defaultRadiusM != 5000 {
		t.Errorf("defaultRadiusM = %d, want 5000", defaultRadiusM)
	}
	if maxRadiusM != 100000 {
		t.Errorf("maxRadiusM = %d, want 100000", maxRadiusM)
	}
}

func TestMigrationConstants(t *testing.T) {
	if !strings.Contains(foursquareBaseURL, "places-api.foursquare.com") {
		t.Errorf("foursquareBaseURL = %q, want places-api.foursquare.com host", foursquareBaseURL)
	}
	if strings.Contains(foursquareBaseURL, "/v3") {
		t.Errorf("foursquareBaseURL = %q, should not contain /v3 (new API uses header versioning)", foursquareBaseURL)
	}
	if len(foursquareAPIVersion) != 10 {
		t.Errorf("foursquareAPIVersion = %q, want YYYY-MM-DD format (10 chars)", foursquareAPIVersion)
	}
}
