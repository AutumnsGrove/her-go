// Package set_location implements the set_location tool — sets the user's
// HOME location so get_weather has coordinates to work with.
//
// This tool writes to THREE places, in order:
//
//  1. integrate.Geocode to resolve the query → (lat, lon, display name)
//  2. ctx.Store.InsertLocation to record the change in location_history
//     (source "text") — this is the DB mirror
//  3. ctx.Cfg.SetLocation to persist coordinates + display name to
//     config.yaml so they survive restarts
//
// The in-memory config is updated as a side effect of step 3 (SetLocation
// mutates the struct before writing the file). That means the next call
// to get_weather in the same turn will pick up the new coordinates.
//
// Geocoding uses integrate.Geocode (Nominatim), which handles cities,
// addresses, landmarks, and raw "lat,lon" strings — strictly better
// than the old Open-Meteo geocoder that only understood city names.
package set_location

import (
	"encoding/json"
	"fmt"

	"her/integrate"
	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/set_location")

func init() {
	tools.Register("set_location", Handle)
}

// Handle geocodes the query and persists the result to both config and
// the DB. Returns a user-friendly confirmation string.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}
	if args.Query == "" {
		return "error: query is required (e.g., 'Portland Oregon' or '40.71,-74.00')"
	}

	// Step 1: geocode.
	loc, err := integrate.Geocode(args.Query)
	if err != nil {
		log.Warn("geocode failed", "query", args.Query, "err", err)
		return fmt.Sprintf("error: couldn't find location for %q: %v", args.Query, err)
	}
	if loc == nil {
		return fmt.Sprintf("error: no location found for %q", args.Query)
	}

	// Step 2: DB mirror. Source "text" matches the location_history
	// convention for geocoded addresses (see memory/store.go). Failure
	// here is logged but doesn't block the update — config.yaml is the
	// source of truth, DB is just an audit trail.
	if ctx.Store != nil {
		if err := ctx.Store.InsertLocation(loc.Latitude, loc.Longitude, loc.DisplayName, "text", ctx.ConversationID); err != nil {
			log.Warn("failed to mirror location to DB", "err", err)
		}
	}

	// Step 3: persist to config.yaml. This also mutates ctx.Cfg in
	// memory, so subsequent tool calls in this turn see the new coords.
	// Failure here means the lat/lon won't survive restart — surface
	// that to the agent so it can tell the user.
	configSaved := true
	if ctx.ConfigPath != "" && ctx.Cfg != nil {
		if err := ctx.Cfg.SetLocation(ctx.ConfigPath, loc.Latitude, loc.Longitude, loc.DisplayName); err != nil {
			log.Warn("failed to persist location to config", "err", err)
			configSaved = false
		}
	}

	log.Info("set_location",
		"query", args.Query,
		"name", loc.DisplayName,
		"lat", loc.Latitude,
		"lon", loc.Longitude)

	if !configSaved {
		return fmt.Sprintf(
			"Location set in-memory to %s (%.4f, %.4f), but couldn't write to config.yaml — may not survive restart.",
			loc.DisplayName, loc.Latitude, loc.Longitude,
		)
	}

	return fmt.Sprintf(
		"Home location set to %s (%.4f, %.4f). Saved to config and location_history. get_weather will now use this.",
		loc.DisplayName, loc.Latitude, loc.Longitude,
	)
}
