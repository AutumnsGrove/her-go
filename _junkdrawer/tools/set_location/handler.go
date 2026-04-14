// Package set_location implements the set_location tool — sets the user's
// location by city name so weather data is available in conversations.
//
// Geocoding converts a city name to lat/lon coordinates via the Open-Meteo
// geocoding API. Coordinates are saved both in-memory (for the current session)
// and to config.yaml (for future restarts).
package set_location

import (
	"encoding/json"
	"fmt"

	"her/logger"
	"her/tools"
	"her/weather"
)

var log = logger.WithPrefix("tools/set_location")

func init() {
	tools.Register("set_location", Handle)
}

// Handle geocodes the query string and updates the weather client's location.
// The coordinates are persisted to config.yaml so they survive restarts.
// On success, future weather fetches use the new location immediately.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}
	if args.Query == "" {
		return "error: query is required (e.g., 'Portland Oregon')"
	}

	// Look up coordinates from the city name via Open-Meteo geocoding.
	loc, err := weather.GeocodeLookup(args.Query)
	if err != nil {
		return fmt.Sprintf("error: couldn't find location for %q: %v", args.Query, err)
	}

	// Update the weather client so future weather fetches use the new location.
	// Nil-safe — the client may not be configured if no weather section exists.
	if ctx.WeatherClient != nil {
		ctx.WeatherClient.SetLocation(loc.Latitude, loc.Longitude)
	}

	// Persist coordinates to config.yaml so they survive restarts.
	// We log a warning on failure but don't return an error — the
	// in-memory update already worked, so weather is live immediately.
	if ctx.ConfigPath != "" {
		if err := ctx.Cfg.SetLocation(ctx.ConfigPath, loc.Latitude, loc.Longitude); err != nil {
			log.Warn("set_location: failed to persist coordinates to config", "err", err)
		}
	}

	log.Info("set_location", "query", args.Query, "lat", loc.Latitude, "lon", loc.Longitude)

	return fmt.Sprintf("Location set to %s, %s, %s. Weather data will now reflect this location. Location saved to config.",
		loc.Name, loc.Region, loc.Country)
}
