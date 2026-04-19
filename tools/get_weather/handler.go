// Package get_weather implements the get_weather tool — fetches current
// conditions from Open-Meteo for either the user's saved home location
// or a one-off place passed in as an argument.
//
// This is a deferred tool: the agent loads it via use_tools(["context"])
// when weather becomes relevant. It's NOT a passive context layer —
// weather is only fetched when the agent explicitly asks for it, which
// keeps the chat prompt clean and avoids spurious API calls.
package get_weather

import (
	"encoding/json"
	"fmt"

	"her/integrate"
	"her/logger"
	"her/tools"
	"her/weather"
)

var log = logger.WithPrefix("tools/get_weather")

func init() {
	tools.Register("get_weather", Handle)
}

// Handle fetches the current weather. The location argument is optional:
// when empty, we use the saved home coords from config; otherwise we
// geocode the string via integrate.Geocode and fetch for those coords
// (without persisting anything — that's set_location's job).
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		Location string `json:"location"`
	}
	// Empty JSON "{}" is valid — no location arg just means "use home".
	if argsJSON != "" {
		if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
			return fmt.Sprintf("error parsing arguments: %v", err)
		}
	}

	lat, lon := 0.0, 0.0
	label := ""

	if args.Location != "" {
		// One-off lookup — geocode without touching config.
		loc, err := integrate.Geocode(args.Location)
		if err != nil {
			log.Warn("geocode failed", "query", args.Location, "err", err)
			return fmt.Sprintf("error: couldn't find location for %q: %v", args.Location, err)
		}
		if loc == nil {
			return fmt.Sprintf("error: no location found for %q", args.Location)
		}
		lat, lon, label = loc.Latitude, loc.Longitude, loc.DisplayName
	} else {
		// Use saved home location from config.
		if ctx.Cfg == nil || (ctx.Cfg.Location.Latitude == 0 && ctx.Cfg.Location.Longitude == 0) {
			return "error: no home location set. Call set_location first (e.g., set_location({query: \"Portland Oregon\"})) or pass a location argument to get_weather."
		}
		lat = ctx.Cfg.Location.Latitude
		lon = ctx.Cfg.Location.Longitude
		label = ctx.Cfg.Location.Name
		if label == "" {
			label = fmt.Sprintf("%.4f, %.4f", lat, lon)
		}
	}

	// Pull unit prefs from config. Empty strings are handled inside Fetch
	// (defaults to fahrenheit/mph), so we can pass them through directly.
	tempUnit, windUnit := "", ""
	if ctx.Cfg != nil {
		tempUnit = ctx.Cfg.Location.TempUnit
		windUnit = ctx.Cfg.Location.WindUnit
	}

	log.Info("get_weather", "location", label, "lat", lat, "lon", lon)

	w, err := weather.Fetch(lat, lon, tempUnit, windUnit)
	if err != nil {
		return fmt.Sprintf("error: weather fetch failed: %v", err)
	}

	// Prefix the location label so the agent can tell the chat model
	// which place these numbers are for — especially important for the
	// one-off case where it's not the user's home.
	return fmt.Sprintf("%s — %s", label, weather.Format(w))
}
