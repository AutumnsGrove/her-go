// integrate/geocode.go — Nominatim (OpenStreetMap) geocoder for converting
// text locations into coordinates. Handles addresses, landmarks, and POIs
// that Open-Meteo's city-level geocoder can't resolve.
//
// Nominatim usage policy: max 1 req/sec, custom User-Agent, no bulk use.
// Fine for personal chatbot use. https://nominatim.org/release-docs/develop/api/Search/
package integrate

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// GeoResult holds a geocoded location — coordinates plus a human-readable
// display name. This is what text locations get resolved to before they
// hit any other API (Foursquare, location_history, etc.).
type GeoResult struct {
	Latitude    float64
	Longitude   float64
	DisplayName string // human-readable: "Brooklyn Public Library, Flatbush Ave, Brooklyn, NY"
}

// nominatimResult maps the JSON response from Nominatim's /search endpoint.
// Only the fields we use — Nominatim returns many more.
type nominatimResult struct {
	Lat         string `json:"lat"`
	Lon         string `json:"lon"`
	DisplayName string `json:"display_name"`
}

// coordPattern matches raw coordinates in various formats:
//   - "40.7128, -74.0060"
//   - "40.7128,-74.0060"
//   - "40.7128 -74.0060"
var coordPattern = regexp.MustCompile(`^(-?\d+\.?\d*)[,\s]+(-?\d+\.?\d*)$`)

// Geocode converts a text location into coordinates. Handles three cases:
//
//  1. Raw coordinates ("40.71, -74.00") → parsed directly, no API call
//  2. Place name / address ("Brooklyn Library") → Nominatim lookup
//  3. Empty string → returns nil (caller should use fallback)
//
// Returns nil if the location can't be resolved.
func Geocode(query string) (*GeoResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}

	// Check if it's raw coordinates first — no API call needed.
	if matches := coordPattern.FindStringSubmatch(query); matches != nil {
		lat, err1 := strconv.ParseFloat(matches[1], 64)
		lon, err2 := strconv.ParseFloat(matches[2], 64)
		if err1 == nil && err2 == nil {
			return &GeoResult{
				Latitude:    lat,
				Longitude:   lon,
				DisplayName: fmt.Sprintf("%.4f, %.4f", lat, lon),
			}, nil
		}
	}

	// Text location — ask Nominatim.
	endpoint := fmt.Sprintf("https://nominatim.openstreetmap.org/search?q=%s&format=json&limit=1",
		url.QueryEscape(query))

	req, err := http.NewRequest("GET", endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("creating geocode request: %w", err)
	}
	// Nominatim requires a custom User-Agent identifying the application.
	// Generic agents get rate-limited or blocked.
	req.Header.Set("User-Agent", "her-go/1.0 (personal companion bot)")
	req.Header.Set("Accept", "application/json")

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("geocoding %q: %w", query, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading geocode response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("nominatim error (status %d): %s", resp.StatusCode, string(body))
	}

	var results []nominatimResult
	if err := json.Unmarshal(body, &results); err != nil {
		return nil, fmt.Errorf("parsing geocode response: %w", err)
	}

	if len(results) == 0 {
		return nil, nil // no results — location not found
	}

	lat, err := strconv.ParseFloat(results[0].Lat, 64)
	if err != nil {
		return nil, fmt.Errorf("parsing latitude: %w", err)
	}
	lon, err := strconv.ParseFloat(results[0].Lon, 64)
	if err != nil {
		return nil, fmt.Errorf("parsing longitude: %w", err)
	}

	return &GeoResult{
		Latitude:    lat,
		Longitude:   lon,
		DisplayName: results[0].DisplayName,
	}, nil
}
