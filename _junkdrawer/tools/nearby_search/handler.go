// Package nearby_search implements the nearby_search tool — finds places
// near a location using Foursquare Places API, with Tavily web search as
// a fallback when Foursquare isn't configured.
//
// Location resolution is flexible: accepts place names, addresses, or raw
// coordinates. Falls back to recent location history, then weather config.
// Every resolved location is recorded in location_history for future analysis.
package nearby_search

import (
	"encoding/json"
	"fmt"

	"her/integrate"
	"her/logger"
	"her/search"
	"her/tools"
)

var log = logger.WithPrefix("tools/nearby_search")

func init() {
	tools.Register("nearby_search", Handle)
}

// Handle searches for nearby places. The location resolution chain:
//  1. Explicit location arg → geocode via Nominatim
//  2. No location → latest entry in location_history
//  3. No history → weather config lat/lon (home base)
//  4. Nothing at all → error
//
// If Foursquare isn't configured, falls back to Tavily web search.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		Query    string  `json:"query"`
		Location string  `json:"location"`
		RadiusKM float64 `json:"radius_km"`
		Limit    int     `json:"limit"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return fmt.Sprintf("error parsing arguments: %v", err)
	}

	if args.Query == "" {
		return "error: query is required (e.g., 'coffee shop', 'pharmacy')"
	}

	// Defaults.
	if args.Limit <= 0 {
		args.Limit = 5
	}
	if args.RadiusKM <= 0 {
		args.RadiusKM = 5
	}
	radiusM := int(args.RadiusKM * 1000)

	// --- Resolve location ---
	var lat, lon float64
	var locationLabel string
	var resolved bool

	// Step 1: Explicit location provided — geocode it.
	if args.Location != "" {
		geo, err := integrate.Geocode(args.Location)
		if err != nil {
			log.Warn("geocoding failed, trying fallbacks", "location", args.Location, "err", err)
		} else if geo != nil {
			lat, lon = geo.Latitude, geo.Longitude
			locationLabel = geo.DisplayName
			resolved = true

			// Record this geocoded location in history.
			if ctx.Store != nil {
				if err := ctx.Store.InsertLocation(lat, lon, locationLabel, "text", ctx.ConversationID); err != nil {
					log.Warn("failed to record location", "err", err)
				}
			}
		}
	}

	// Step 2: No explicit location — check recent history.
	if !resolved && ctx.Store != nil {
		if recent := ctx.Store.LatestLocation(); recent != nil {
			lat, lon = recent.Latitude, recent.Longitude
			locationLabel = recent.Label
			if locationLabel == "" {
				locationLabel = fmt.Sprintf("%.4f, %.4f", lat, lon)
			}
			resolved = true
			log.Infof("  using recent location: %s", locationLabel)
		}
	}

	// Step 3: No history — fall back to weather config (home base).
	if !resolved && ctx.Cfg != nil {
		if ctx.Cfg.Weather.Latitude != 0 || ctx.Cfg.Weather.Longitude != 0 {
			lat = ctx.Cfg.Weather.Latitude
			lon = ctx.Cfg.Weather.Longitude
			locationLabel = "home location"
			resolved = true
			log.Info("  using weather config location as fallback")
		}
	}

	// --- Search ---

	// Try Foursquare first (structured results with distance, categories).
	fsqClient := integrate.NewFoursquareClient(ctx.Cfg.Foursquare.APIKey)

	if fsqClient != nil && resolved {
		places, err := fsqClient.SearchNearby(lat, lon, args.Query, radiusM, args.Limit)
		if err != nil {
			log.Error("foursquare search failed, falling back to web search", "err", err)
			// Fall through to Tavily fallback below.
		} else {
			// Record the search location in history.
			if ctx.Store != nil {
				if err := ctx.Store.InsertLocation(lat, lon, args.Query+" near "+locationLabel, "search", ctx.ConversationID); err != nil {
					log.Warn("failed to record search location", "err", err)
				}
			}

			log.Infof("  nearby_search: %q near %s → %d results (foursquare)", args.Query, locationLabel, len(places))

			result := integrate.FormatPlaces(places)
			if locationLabel != "" {
				result = fmt.Sprintf("*Searching near: %s*\n\n%s", locationLabel, result)
			}
			return result
		}
	}

	// Fallback: Tavily web search with location context.
	// This works surprisingly well for casual queries like
	// "coffee shops near Brooklyn Library."
	if ctx.TavilyClient != nil {
		searchQuery := args.Query
		if args.Location != "" {
			searchQuery += " near " + args.Location
		} else if locationLabel != "" {
			searchQuery += " near " + locationLabel
		}

		log.Infof("  nearby_search: falling back to Tavily for %q", searchQuery)

		resp, err := ctx.TavilyClient.Search(searchQuery, args.Limit)
		if err != nil {
			return fmt.Sprintf("error searching: %v", err)
		}

		// Accumulate in search context so the reply tool can reference it.
		formatted := "## Nearby Places (web search)\n\n" + search.FormatSearchResults(resp)
		ctx.SearchContext += formatted + "\n\n"
		return formatted
	}

	// Neither Foursquare nor Tavily configured.
	if !resolved {
		return "Cannot search: no location available. Share your location or tell me where to search."
	}
	return "Cannot search: neither Foursquare nor Tavily is configured. Add foursquare.api_key or search.tavily_api_key to config.yaml."
}
