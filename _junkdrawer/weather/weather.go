// Package weather fetches current weather data from the Open-Meteo API.
//
// Open-Meteo is a free, no-API-key weather service. We hit it once per
// hour (configurable) and cache the result in memory. The cached weather
// gets injected into the system prompt so Mira knows what it's like
// outside — she can reference it naturally without the user asking.
//
// This is a "passive context" pattern: the agent never calls a weather
// tool. Instead, weather data is always available in the prompt, like
// mood data or memory. Mira weaves it in when relevant.
package weather

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"her/logger"
)

var log = logger.WithPrefix("weather")

// Client fetches and caches current weather from Open-Meteo.
// Safe for concurrent use — the cache is protected by a mutex.
type Client struct {
	latitude  float64
	longitude float64
	tempUnit  string // "fahrenheit" or "celsius"
	windUnit  string // "mph" or "kmh"
	cacheTTL  time.Duration
	http      *http.Client

	// In-memory cache. The mutex protects both fields.
	// sync.Mutex is Go's basic lock — like threading.Lock() in Python.
	// Lock() acquires, Unlock() releases. Always defer Unlock() so it
	// runs even if the function returns early or panics.
	mu     sync.Mutex
	cached *CurrentWeather
}

// CurrentWeather holds the latest weather data.
type CurrentWeather struct {
	Temperature float64   // e.g., 54.2
	TempUnit    string    // "°F" or "°C"
	WeatherCode int       // WMO code (0-99)
	Description string    // human-readable, e.g., "partly cloudy"
	Precip      float64   // precipitation in mm
	WindSpeed   float64   // e.g., 12.3
	WindUnit    string    // "mph" or "km/h"
	FetchedAt   time.Time // when this data was fetched
}

// NewClient creates a weather client. Returns nil if no location is
// configured (lat/lon both zero).
//
// tempUnit: "fahrenheit" (default) or "celsius"
// windUnit: "mph" (default) or "kmh"
// cacheTTLSeconds: how long to cache results (default 3600 = 1 hour)
func NewClient(lat, lon float64, tempUnit, windUnit string, cacheTTLSeconds int) *Client {
	if lat == 0 && lon == 0 {
		return nil
	}

	if tempUnit == "" {
		tempUnit = "fahrenheit"
	}
	if windUnit == "" {
		windUnit = "mph"
	}
	if cacheTTLSeconds <= 0 {
		cacheTTLSeconds = 3600
	}

	return &Client{
		latitude:  lat,
		longitude: lon,
		tempUnit:  tempUnit,
		windUnit:  windUnit,
		cacheTTL:  time.Duration(cacheTTLSeconds) * time.Second,
		http: &http.Client{
			Timeout: 15 * time.Second,
		},
	}
}

// openMeteoResponse is the JSON shape returned by the Open-Meteo
// /v1/forecast endpoint with the "current" parameter.
type openMeteoResponse struct {
	Current struct {
		Temperature float64 `json:"temperature_2m"`
		WeatherCode int     `json:"weather_code"`
		Precip      float64 `json:"precipitation"`
		WindSpeed   float64 `json:"wind_speed_10m"`
	} `json:"current"`
}

// GetCurrent returns the current weather, using the cache if fresh
// enough. Thread-safe — multiple goroutines can call this concurrently.
func (c *Client) GetCurrent() (*CurrentWeather, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Return cached data if it's fresh enough.
	if c.cached != nil && time.Since(c.cached.FetchedAt) < c.cacheTTL {
		return c.cached, nil
	}

	// Cache miss or expired — fetch fresh data.
	weather, err := c.fetch()
	if err != nil {
		// If we have stale data, return it rather than nothing.
		// Better to say "54°F" from an hour ago than have no weather.
		if c.cached != nil {
			log.Warn("using stale weather data", "err", err,
				"age", time.Since(c.cached.FetchedAt).Round(time.Minute))
			return c.cached, nil
		}
		return nil, err
	}

	c.cached = weather
	return weather, nil
}

// fetch makes the actual HTTP request to Open-Meteo.
func (c *Client) fetch() (*CurrentWeather, error) {
	url := fmt.Sprintf(
		"https://api.open-meteo.com/v1/forecast?latitude=%.4f&longitude=%.4f"+
			"&current=temperature_2m,weather_code,precipitation,wind_speed_10m"+
			"&temperature_unit=%s&wind_speed_unit=%s",
		c.latitude, c.longitude, c.tempUnit, c.windUnit,
	)

	resp, err := c.http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("fetching weather: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("Open-Meteo returned %d: %s", resp.StatusCode, string(body))
	}

	var result openMeteoResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("parsing weather response: %w", err)
	}

	// Build the unit display strings.
	tempUnitDisplay := "°F"
	if c.tempUnit == "celsius" {
		tempUnitDisplay = "°C"
	}
	windUnitDisplay := "mph"
	if c.windUnit == "kmh" {
		windUnitDisplay = "km/h"
	}

	weather := &CurrentWeather{
		Temperature: result.Current.Temperature,
		TempUnit:    tempUnitDisplay,
		WeatherCode: result.Current.WeatherCode,
		Description: wmoDescription(result.Current.WeatherCode),
		Precip:      result.Current.Precip,
		WindSpeed:    result.Current.WindSpeed,
		WindUnit:     windUnitDisplay,
		FetchedAt:   time.Now(),
	}

	log.Info("weather fetched",
		"temp", fmt.Sprintf("%.0f%s", weather.Temperature, weather.TempUnit),
		"condition", weather.Description,
		"wind", fmt.Sprintf("%.0f %s", weather.WindSpeed, weather.WindUnit),
	)

	return weather, nil
}

// FormatContext returns a short weather summary for injection into the
// system prompt. Returns "" if no data is available.
//
// Example: "Current weather: 54°F, partly cloudy, no precipitation, wind 12 mph"
func (c *Client) FormatContext() string {
	weather, err := c.GetCurrent()
	if err != nil || weather == nil {
		return ""
	}

	precipStr := "no precipitation"
	if weather.Precip > 0 {
		precipStr = fmt.Sprintf("%.1f mm precipitation", weather.Precip)
	}

	return fmt.Sprintf("Current weather: %.0f%s, %s, %s, wind %.0f %s",
		weather.Temperature, weather.TempUnit,
		weather.Description,
		precipStr,
		weather.WindSpeed, weather.WindUnit,
	)
}

// SetLocation updates the client's coordinates and clears the cache
// so the next fetch gets weather for the new location. Called by the
// set_location agent tool after geocoding a city name.
func (c *Client) SetLocation(lat, lon float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.latitude = lat
	c.longitude = lon
	c.cached = nil // force re-fetch for new location
	log.Info("location updated", "lat", lat, "lon", lon)
}

// GeoLocation holds the result of a geocoding lookup.
type GeoLocation struct {
	Name      string  // city name
	Region    string  // state/province
	Country   string  // country name
	Latitude  float64
	Longitude float64
}

// geocodingResponse is the JSON shape from the Open-Meteo geocoding API.
type geocodingResponse struct {
	Results []struct {
		Name      string  `json:"name"`
		Latitude  float64 `json:"latitude"`
		Longitude float64 `json:"longitude"`
		Country   string  `json:"country"`
		Admin1    string  `json:"admin1"` // state/province
	} `json:"results"`
}

// GeocodeLookup searches for a location by name using the Open-Meteo
// geocoding API (free, no key needed). Returns the top result.
// This is a package-level function — it doesn't need a Client since
// geocoding is a one-off operation independent of weather fetching.
func GeocodeLookup(query string) (*GeoLocation, error) {
	// URL-encode the query so "Portland Oregon" becomes "Portland%20Oregon".
	// Without this, spaces break the request.
	reqURL := fmt.Sprintf(
		"https://geocoding-api.open-meteo.com/v1/search?name=%s&count=5&language=en",
		url.QueryEscape(query),
	)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(reqURL)
	if err != nil {
		return nil, fmt.Errorf("geocoding lookup: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("geocoding API returned %d: %s", resp.StatusCode, string(body))
	}

	var result geocodingResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("parsing geocoding response: %w", err)
	}

	// If no results with the full query (e.g., "Portland Oregon"),
	// try just the first word (e.g., "Portland"). The API treats the
	// name parameter as a single search term, not "city + state".
	if len(result.Results) == 0 {
		parts := strings.Fields(query)
		if len(parts) > 1 {
			return GeocodeLookup(parts[0])
		}
		return nil, fmt.Errorf("no results found for %q", query)
	}

	r := result.Results[0]
	return &GeoLocation{
		Name:      r.Name,
		Region:    r.Admin1,
		Country:   r.Country,
		Latitude:  r.Latitude,
		Longitude: r.Longitude,
	}, nil
}

// wmoDescription translates WMO weather codes to human-readable
// descriptions. These are the standard codes used by Open-Meteo
// and other weather services worldwide.
//
// Full table: https://www.nodc.noaa.gov/archive/arc0021/0002199/1.1/data/0-data/HTML/WMO-CODE/WMO4677.HTM
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
