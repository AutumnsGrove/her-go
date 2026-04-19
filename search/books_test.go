package search

// Tests for FormatBookResults — the pure formatting function in books.go.
// SearchBooks itself makes HTTP calls to Open Library, so we test that
// separately if/when we add httptest.Server coverage. For now, the
// formatting logic has enough branching to be worth covering.

import (
	"strings"
	"testing"
)

// TestFormatBookResults exercises the formatter with various result shapes:
// empty, single, multiple, missing fields. Table-driven, same pattern as
// the config tests.
func TestFormatBookResults(t *testing.T) {
	tests := []struct {
		name         string
		books        []BookResult
		wantContains []string
		wantAbsent   []string
	}{
		{
			name:         "empty results",
			books:        nil,
			wantContains: []string{"No books found."},
		},
		{
			name: "single book with all fields",
			books: []BookResult{
				{
					Title:          "The Overstory",
					Authors:        []string{"Richard Powers"},
					FirstPublished: 2018,
					Subjects:       []string{"Fiction", "Trees", "Environment"},
					PageCount:      502,
					CoverURL:       "https://covers.openlibrary.org/b/id/12345-M.jpg",
				},
			},
			wantContains: []string{
				"1. **The Overstory**",
				"by Richard Powers",
				"(2018)",
				"Pages: 502",
				"Subjects: Fiction, Trees, Environment",
			},
		},
		{
			name: "book with no authors or year",
			books: []BookResult{
				{
					Title: "Anonymous Pamphlet",
				},
			},
			wantContains: []string{"1. **Anonymous Pamphlet**"},
			wantAbsent:   []string{"by ", "(0)", "Pages:", "Subjects:"},
		},
		{
			name: "multiple authors",
			books: []BookResult{
				{
					Title:   "Good Omens",
					Authors: []string{"Terry Pratchett", "Neil Gaiman"},
				},
			},
			wantContains: []string{"by Terry Pratchett, Neil Gaiman"},
		},
		{
			name: "multiple books are numbered",
			books: []BookResult{
				{Title: "First"},
				{Title: "Second"},
				{Title: "Third"},
			},
			wantContains: []string{
				"1. **First**",
				"2. **Second**",
				"3. **Third**",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := FormatBookResults(tc.books)

			for _, want := range tc.wantContains {
				if !strings.Contains(got, want) {
					t.Errorf("output missing %q\n--- got ---\n%s", want, got)
				}
			}
			for _, absent := range tc.wantAbsent {
				if strings.Contains(got, absent) {
					t.Errorf("output should not contain %q\n--- got ---\n%s", absent, got)
				}
			}
		})
	}
}
