// integrate/timezone.go — Timezone lookup from coordinates via TimeAPI.io.
// Free, no API key needed. Used by set_location to auto-detect timezone
// when the user sets their home location.
package integrate

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// TimezoneFromCoords looks up the IANA timezone name (e.g., "America/New_York")
// for a given latitude/longitude pair using TimeAPI.io.
func TimezoneFromCoords(lat, lon float64) (string, error) {
	endpoint := fmt.Sprintf(
		"https://timeapi.io/api/timezone/coordinate?latitude=%.6f&longitude=%.6f",
		lat, lon,
	)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(endpoint)
	if err != nil {
		return "", fmt.Errorf("timezone lookup: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading timezone response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("timezone API error (status %d): %s", resp.StatusCode, string(body))
	}

	var result struct {
		TimeZone string `json:"timeZone"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("parsing timezone response: %w", err)
	}

	if result.TimeZone == "" {
		return "", fmt.Errorf("timezone API returned empty timezone for (%.4f, %.4f)", lat, lon)
	}

	return result.TimeZone, nil
}
