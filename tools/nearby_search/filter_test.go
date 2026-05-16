package nearby_search

import (
	"testing"

	"her/integrate"
)

func TestFilterByRelevance(t *testing.T) {
	coffee := integrate.Place{
		Name:       "Blue Bottle Coffee",
		Categories: []integrate.PlaceCategory{{Name: "Coffee Shop"}},
	}
	barber := integrate.Place{
		Name:       "Joe's Barber",
		Categories: []integrate.PlaceCategory{{Name: "Barber Shop"}},
	}
	bakery := integrate.Place{
		Name:       "Coffee Cake Bakery",
		Categories: []integrate.PlaceCategory{{Name: "Bakery"}},
	}
	unlabeled := integrate.Place{
		Name:       "The Coffee House",
		Categories: nil,
	}

	tests := []struct {
		name   string
		places []integrate.Place
		query  string
		want   int
	}{
		{
			name:   "filters out barber from coffee query",
			places: []integrate.Place{coffee, barber, bakery},
			query:  "coffee",
			want:   2, // Blue Bottle + Coffee Cake (name match)
		},
		{
			name:   "name match keeps unlabeled places",
			places: []integrate.Place{coffee, unlabeled, barber},
			query:  "coffee",
			want:   2, // Blue Bottle + The Coffee House
		},
		{
			name:   "empty query returns all",
			places: []integrate.Place{coffee, barber},
			query:  "",
			want:   2,
		},
		{
			name:   "all filtered returns original (fail-open)",
			places: []integrate.Place{barber},
			query:  "sushi restaurant",
			want:   1, // barber returned since filtering removed everything
		},
		{
			name:   "empty places returns empty",
			places: []integrate.Place{},
			query:  "coffee",
			want:   0,
		},
		{
			name:   "case insensitive matching",
			places: []integrate.Place{coffee, barber},
			query:  "COFFEE",
			want:   1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := filterByRelevance(tt.places, tt.query)
			if len(got) != tt.want {
				t.Errorf("filterByRelevance(%d places, %q) returned %d, want %d",
					len(tt.places), tt.query, len(got), tt.want)
			}
		})
	}
}

func TestPlaceMatchesQuery(t *testing.T) {
	place := integrate.Place{
		Name:       "Starbucks Reserve",
		Categories: []integrate.PlaceCategory{{Name: "Coffee Shop"}, {Name: "Café"}},
	}

	tests := []struct {
		words []string
		want  bool
	}{
		{[]string{"coffee"}, true},
		{[]string{"café"}, true},
		{[]string{"starbucks"}, true},
		{[]string{"reserve"}, true},
		{[]string{"barber"}, false},
		{[]string{"pizza", "restaurant"}, false},
	}

	for _, tt := range tests {
		got := placeMatchesQuery(place, tt.words)
		if got != tt.want {
			t.Errorf("placeMatchesQuery(Starbucks, %v) = %v, want %v", tt.words, got, tt.want)
		}
	}
}
