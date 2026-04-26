// integrate/foursquare.go — Foursquare Places API client for nearby place
// search. Returns structured results (name, address, distance, categories)
// for queries like "coffee shops near me."
//
// Free tier: 10,000 calls/month. Auth: Bearer token with a Service API Key
// (generate one at https://foursquare.com/developers → Settings → Service API Keys).
//
// The API migrated from api.foursquare.com/v3 to places-api.foursquare.com
// in early 2026. Date-based versioning is set via X-Places-Api-Version header.
//
// Docs: https://docs.foursquare.com/developer/reference/place-search
package integrate

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// FoursquareClient wraps the Foursquare Places API for nearby search.
type FoursquareClient struct {
	apiKey string
	http   *http.Client
}

// NewFoursquareClient creates a Foursquare Places API client.
// Returns nil if apiKey is empty (not configured).
func NewFoursquareClient(apiKey string) *FoursquareClient {
	if apiKey == "" {
		return nil
	}
	return &FoursquareClient{
		apiKey: apiKey,
		http:   &http.Client{Timeout: 15 * time.Second},
	}
}

// foursquareBaseURL is the new Places API host. The old host
// (api.foursquare.com/v3) returns 410 Gone as of April 2026.
const foursquareBaseURL = "https://places-api.foursquare.com"

// foursquareAPIVersion is the date-based version sent in the
// X-Places-Api-Version header. Foursquare uses this instead of
// URL path versioning (/v3/ is gone). Pin to a known-good date
// and bump deliberately when adopting new API features.
const foursquareAPIVersion = "2025-06-17"

const (
	// defaultRadiusM is the default search radius when the caller passes 0.
	defaultRadiusM = 5000 // 5km — reasonable walking/driving distance

	// maxRadiusM is Foursquare's Places API maximum allowed search radius.
	// Requests above this are clamped silently rather than erroring.
	maxRadiusM = 100000 // 100km — API ceiling
)

// ---------------------------------------------------------------------------
// Response types — only the fields we use from Foursquare's response.
// The API returns much more, but Go's JSON decoder silently ignores
// unknown fields, so we only map what we need.
// ---------------------------------------------------------------------------

// Place represents a single result from Foursquare place search.
// Coordinates are top-level fields (latitude/longitude) on the new
// Places API — the old nested geocodes.main structure is gone.
type Place struct {
	FSQPlaceID string          `json:"fsq_place_id"` // unique place identifier (was fsq_id on legacy API)
	Name       string          `json:"name"`
	Location   PlaceLocation   `json:"location"`
	Categories []PlaceCategory `json:"categories"`
	Distance   int             `json:"distance"`  // meters from search center (only with ll param)
	Latitude   float64         `json:"latitude"`  // place coordinates (top-level on new API)
	Longitude  float64         `json:"longitude"`
}

// PlaceLocation holds address info for a place.
type PlaceLocation struct {
	Address          string `json:"address"`
	FormattedAddress string `json:"formatted_address"`
	Locality         string `json:"locality"` // city
	Region           string `json:"region"`   // state
	Country          string `json:"country"`
}

// PlaceCategory is a Foursquare category (e.g., "Coffee Shop", "Bookstore").
type PlaceCategory struct {
	Name string `json:"name"`
}

// placeSearchResponse wraps the results array from Foursquare.
type placeSearchResponse struct {
	Results []Place `json:"results"`
}

// ---------------------------------------------------------------------------
// API methods
// ---------------------------------------------------------------------------

// SearchNearby finds places near the given coordinates matching the query.
// Returns up to limit results sorted by distance.
//
// The ll (lat/lon) parameter is always used (not "near") because it
// ensures the distance field is populated in results. The caller should
// geocode text locations before calling this method.
func (c *FoursquareClient) SearchNearby(lat, lon float64, query string, radiusM, limit int) ([]Place, error) {
	if limit <= 0 || limit > 50 {
		limit = 10
	}
	if radiusM <= 0 {
		radiusM = defaultRadiusM
	}
	if radiusM > maxRadiusM {
		radiusM = maxRadiusM
	}

	// Build the request URL with query parameters.
	// Note: no /v3/ path segment — the new host uses date-based versioning
	// via the X-Places-Api-Version header instead.
	endpoint := fmt.Sprintf("%s/places/search?ll=%f,%f&radius=%d&limit=%d&sort=DISTANCE",
		foursquareBaseURL, lat, lon, radiusM, limit)

	if query != "" {
		endpoint += "&query=" + url.QueryEscape(query)
	}

	// Request specific fields to keep the response lean.
	endpoint += "&fields=fsq_place_id,name,location,categories,distance,latitude,longitude"

	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("creating request: %w", err)
	}
	c.setAuth(req)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("searching places: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("foursquare error (status %d): %s", resp.StatusCode, string(body))
	}

	var searchResp placeSearchResponse
	if err := json.Unmarshal(body, &searchResp); err != nil {
		return nil, fmt.Errorf("parsing places: %w", err)
	}

	return searchResp.Results, nil
}

// ---------------------------------------------------------------------------
// Formatting helpers
// ---------------------------------------------------------------------------

// FormatPlaces turns search results into a compact summary for the agent's
// reasoning context (SearchContext). This is NOT the user-facing output —
// the pretty place cards are built separately via tools.PlaceCard and
// appended by the reply tool. This summary gives the agent enough info
// to decide which places to recommend and what to say about them.
func FormatPlaces(places []Place) string {
	if len(places) == 0 {
		return "No places found nearby."
	}

	var b strings.Builder
	for i, p := range places {
		dist := formatDistance(p.Distance)
		cats := JoinCategories(p.Categories)

		fmt.Fprintf(&b, "%d. %s", i+1, p.Name)
		if cats != "" {
			fmt.Fprintf(&b, " (%s)", cats)
		}
		fmt.Fprintf(&b, " — %s", dist)

		addr := PlaceAddress(p)
		if addr != "" {
			fmt.Fprintf(&b, " | %s", addr)
		}
		b.WriteString("\n")
	}

	return b.String()
}

// JoinCategories combines a place's categories into a comma-separated string.
// Returns empty string for no categories.
func JoinCategories(cats []PlaceCategory) string {
	if len(cats) == 0 {
		return ""
	}
	names := make([]string, len(cats))
	for i, c := range cats {
		names[i] = c.Name
	}
	return strings.Join(names, ", ")
}

// PlaceAddress returns the best available address for a place.
// Prefers FormattedAddress, falls back to Address.
func PlaceAddress(p Place) string {
	if p.Location.FormattedAddress != "" {
		return p.Location.FormattedAddress
	}
	return p.Location.Address
}

// PlaceMapsURL builds a Google Maps link from a place's coordinates.
// Returns empty string if no coordinates are available.
func PlaceMapsURL(p Place) string {
	if p.Latitude == 0 && p.Longitude == 0 {
		return ""
	}
	return fmt.Sprintf("https://maps.google.com/?q=%f,%f", p.Latitude, p.Longitude)
}

// FormatDistance is the exported version of formatDistance — used by the
// nearby_search handler when building PlaceCards.
func FormatDistance(meters int) string {
	return formatDistance(meters)
}

// formatDistance converts meters to a human-friendly string.
func formatDistance(meters int) string {
	if meters <= 0 {
		return "nearby"
	}
	if meters < 1000 {
		return fmt.Sprintf("%dm away", meters)
	}
	// Convert to approximate walk time (assuming ~80m/min walking speed).
	walkMins := int(math.Round(float64(meters) / 80.0))
	km := float64(meters) / 1000.0
	if walkMins <= 30 {
		return fmt.Sprintf("%.1fkm (~%d min walk)", km, walkMins)
	}
	return fmt.Sprintf("%.1fkm away", km)
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// setAuth adds Foursquare authentication and versioning headers.
// The new Places API uses Bearer token auth with a Service API Key
// and date-based versioning via X-Places-Api-Version.
func (c *FoursquareClient) setAuth(req *http.Request) {
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("X-Places-Api-Version", foursquareAPIVersion)
	req.Header.Set("Accept", "application/json")
}
