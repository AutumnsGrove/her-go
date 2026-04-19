// Package weather fetches current weather data from the Open-Meteo API.
//
// Open-Meteo is free, no API key, no registration. We make one HTTP call
// per agent decision — the deferred get_weather tool is only invoked when
// the model explicitly asks about the weather, so caching doesn't pay for
// itself. If we ever revive the "passive weather context" layer (where
// weather is injected into every prompt), we'd add a cache.
//
// Geocoding (city name → lat/lon) is handled by integrate.Geocode, which
// uses Nominatim and handles richer input than Open-Meteo's city-level
// geocoder. This package stays focused on the weather fetch itself.
package weather

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"her/logger"
)

var log = logger.WithPrefix("weather")

// Current holds the fields we display for a single weather snapshot.
// The struct is intentionally flat — no nested "current" wrapper like
// the raw Open-Meteo response. Downstream (the get_weather tool, and
// any future passive-context layer) just needs the primitives.
type Current struct {
	Temperature float64 // e.g., 54.2
	TempUnit    string  // "°F" or "°C"
	WeatherCode int     // WMO code (0-99), https://open-meteo.com/en/docs
	Description string  // human-readable: "partly cloudy"
	Precip      float64 // precipitation in mm (Open-Meteo always reports mm)
	WindSpeed   float64
	WindUnit    string   // "mph" or "km/h"
	FetchedAt   time.Time
}

// openMeteoResponse mirrors the JSON returned by Open-Meteo's
// /v1/forecast endpoint with the "current" parameter. We keep the
// struct unexported — callers only see the clean Current type.
type openMeteoResponse struct {
	Current struct {
		Temperature float64 `json:"temperature_2m"`
		WeatherCode int     `json:"weather_code"`
		Precip      float64 `json:"precipitation"`
		WindSpeed   float64 `json:"wind_speed_10m"`
	} `json:"current"`
}

// Fetch calls Open-Meteo for the current conditions at lat/lon. tempUnit
// is "fahrenheit" or "celsius"; windUnit is "mph" or "kmh". Empty strings
// default to fahrenheit/mph.
//
// Note: Open-Meteo's API uses "kmh" (no slash) in the URL, but we display
// "km/h" for readability — hence the separate "display" unit strings
// below.
func Fetch(lat, lon float64, tempUnit, windUnit string) (*Current, error) {
	// Whitelist valid unit values. Open-Meteo only understands these
	// exact strings in the URL — anything else would silently return
	// default units, and a malformed config value could inject URL
	// parameters via &-separation. Belt and suspenders.
	if tempUnit != "fahrenheit" && tempUnit != "celsius" {
		tempUnit = "fahrenheit"
	}
	if windUnit != "mph" && windUnit != "kmh" {
		windUnit = "mph"
	}

	// Build the URL. %.4f gives ~11m precision — plenty for weather and
	// avoids URL bloat from trailing zeros in %f.
	url := fmt.Sprintf(
		"https://api.open-meteo.com/v1/forecast?latitude=%.4f&longitude=%.4f"+
			"&current=temperature_2m,weather_code,precipitation,wind_speed_10m"+
			"&temperature_unit=%s&wind_speed_unit=%s",
		lat, lon, tempUnit, windUnit,
	)

	// Short-lived http.Client — same pattern Tavily uses. In Go the zero
	// value of http.Client works, but setting a Timeout is important:
	// without it, a hung server can stall the agent forever.
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetching weather: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("open-meteo returned %d: %s", resp.StatusCode, string(body))
	}

	var result openMeteoResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("parsing weather response: %w", err)
	}

	tempDisplay := "°F"
	if tempUnit == "celsius" {
		tempDisplay = "°C"
	}
	windDisplay := "mph"
	if windUnit == "kmh" {
		windDisplay = "km/h"
	}

	w := &Current{
		Temperature: result.Current.Temperature,
		TempUnit:    tempDisplay,
		WeatherCode: result.Current.WeatherCode,
		Description: wmoDescription(result.Current.WeatherCode),
		Precip:      result.Current.Precip,
		WindSpeed:   result.Current.WindSpeed,
		WindUnit:    windDisplay,
		FetchedAt:   time.Now(),
	}

	log.Info("weather fetched",
		"temp", fmt.Sprintf("%.0f%s", w.Temperature, w.TempUnit),
		"condition", w.Description,
		"wind", fmt.Sprintf("%.0f %s", w.WindSpeed, w.WindUnit))

	return w, nil
}

// Format renders a Current into a single readable line. Used by the
// get_weather tool; also suitable for a future passive-context layer.
//
// Example: "54°F, partly cloudy, no precipitation, wind 12 mph"
func Format(w *Current) string {
	if w == nil {
		return ""
	}
	precip := "no precipitation"
	if w.Precip > 0 {
		precip = fmt.Sprintf("%.1f mm precipitation", w.Precip)
	}
	return fmt.Sprintf("%.0f%s, %s, %s, wind %.0f %s",
		w.Temperature, w.TempUnit,
		w.Description,
		precip,
		w.WindSpeed, w.WindUnit)
}

// wmoDescription translates WMO weather codes into human-readable
// phrases. These codes are a worldwide standard; Open-Meteo uses them
// and so do most other weather APIs.
//
// Full table: https://open-meteo.com/en/docs (search "WMO Weather interpretation codes")
func wmoDescription(code int) string {
	switch {
	case code == 0:
		return "clear sky"
	case code == 1:
		return "mainly clear"
	case code == 2:
		return "partly cloudy"
	case code == 3:
		return "overcast"
	case code == 45 || code == 48:
		return "foggy"
	case code >= 51 && code <= 57:
		return "drizzle"
	case code >= 61 && code <= 65:
		return "rain"
	case code == 66 || code == 67:
		return "freezing rain"
	case code >= 71 && code <= 77:
		return "snow"
	case code >= 80 && code <= 82:
		return "rain showers"
	case code == 85 || code == 86:
		return "snow showers"
	case code >= 95:
		return "thunderstorm"
	default:
		return "unknown"
	}
}
