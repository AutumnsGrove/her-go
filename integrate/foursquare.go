// integrate/foursquare.go — Foursquare Places API v3 client for nearby
// place search. Returns structured results (name, address, distance,
// categories, open/closed estimate) for queries like "coffee shops near me."
//
// Free tier: 10,000 calls/month. Auth: raw API key in Authorization header
// (no "Bearer" prefix — Foursquare's quirk).
//
// Docs: https://docs.foursquare.com/developer/reference/place-search
package integrate

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"
	"time"
)

// FoursquareClient wraps the Foursquare Places API v3 for nearby search.
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

const foursquareBaseURL = "https://api.foursquare.com/v3"

// ---------------------------------------------------------------------------
// Response types — only the fields we use from Foursquare's response.
// The API returns much more, but Go's JSON decoder silently ignores
// unknown fields, so we only map what we need.
// ---------------------------------------------------------------------------

// Place represents a single result from Foursquare place search.
type Place struct {
	FSQID      string          `json:"fsq_id"`
	Name       string          `json:"name"`
	Location   PlaceLocation   `json:"location"`
	Categories []PlaceCategory `json:"categories"`
	Distance   int             `json:"distance"`      // meters from search center (only with ll param)
	ClosedBucket string        `json:"closed_bucket"` // "VeryLikelyOpen", "LikelyOpen", "LikelyClosed", "VeryLikelyClosed", "Unsure"
	Geocodes   PlaceGeocodes   `json:"geocodes"`
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

// PlaceGeocodes holds coordinate data for a place.
type PlaceGeocodes struct {
	Main struct {
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
	} `json:"main"`
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
		radiusM = 5000 // 5km default
	}
	if radiusM > 100000 {
		radiusM = 100000 // Foursquare max
	}

	// Build the request URL with query parameters.
	endpoint := fmt.Sprintf("%s/places/search?ll=%f,%f&radius=%d&limit=%d&sort=DISTANCE",
		foursquareBaseURL, lat, lon, radiusM, limit)

	if query != "" {
		endpoint += "&query=" + query
	}

	// Request specific fields to keep the response lean.
	// These are all free-tier "Pro" fields.
	endpoint += "&fields=fsq_id,name,location,categories,distance,closed_bucket,geocodes"

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

// FormatPlaces turns search results into readable markdown for the agent.
// Includes distance in human-friendly units, categories, address, and
// open/closed status.
func FormatPlaces(places []Place) string {
	if len(places) == 0 {
		return "No places found nearby."
	}

	var b strings.Builder
	for i, p := range places {
		// Distance in human-friendly format.
		dist := formatDistance(p.Distance)

		// Categories as a short label.
		cats := ""
		if len(p.Categories) > 0 {
			names := make([]string, len(p.Categories))
			for j, c := range p.Categories {
				names[j] = c.Name
			}
			cats = " (" + strings.Join(names, ", ") + ")"
		}

		// Open/closed indicator.
		status := formatOpenStatus(p.ClosedBucket)

		fmt.Fprintf(&b, "%d. **%s**%s%s — %s", i+1, p.Name, cats, status, dist)

		// Address.
		addr := p.Location.FormattedAddress
		if addr == "" {
			addr = p.Location.Address
		}
		if addr != "" {
			fmt.Fprintf(&b, "\n   %s", addr)
		}

		b.WriteString("\n")
	}

	return b.String()
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

// formatOpenStatus turns Foursquare's closed_bucket into a short indicator.
func formatOpenStatus(bucket string) string {
	switch bucket {
	case "VeryLikelyOpen":
		return " [open]"
	case "LikelyOpen":
		return " [likely open]"
	case "LikelyClosed":
		return " [likely closed]"
	case "VeryLikelyClosed":
		return " [closed]"
	default:
		return "" // "Unsure" or empty — don't show anything
	}
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

// setAuth adds Foursquare authentication headers. Note: no "Bearer" prefix —
// Foursquare expects the raw API key directly in the Authorization header.
func (c *FoursquareClient) setAuth(req *http.Request) {
	req.Header.Set("Authorization", c.apiKey)
	req.Header.Set("Accept", "application/json")
}
