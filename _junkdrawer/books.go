// Package search provides web search (Tavily) and book search (Open Library)
// clients for grounding Mira's responses in real-world data.
package search

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// BookResult holds the key info about a book from Open Library.
type BookResult struct {
	Title         string   `json:"title"`
	Authors       []string `json:"authors"`
	FirstPublished int     `json:"first_publish_year"`
	Description   string   `json:"description"`
	Subjects      []string `json:"subjects"`
	PageCount     int      `json:"number_of_pages"`
	CoverURL      string   `json:"cover_url"`
}

// openLibraryResponse is the raw JSON shape from the Open Library search API.
type openLibraryResponse struct {
	NumFound int `json:"numFound"`
	Docs     []struct {
		Title            string   `json:"title"`
		AuthorName       []string `json:"author_name"`
		FirstPublishYear int      `json:"first_publish_year"`
		Subject          []string `json:"subject"`
		CoverI           int      `json:"cover_i"`
		NumberOfPages    int      `json:"number_of_pages_median"`
	} `json:"docs"`
}

// SearchBooks queries the Open Library API for books matching the query.
// Returns up to maxResults books. Open Library is free, no API key needed.
//
// The API is simple: GET https://openlibrary.org/search.json?q=query&limit=5
// Similar to how you'd use requests.get() in Python.
func SearchBooks(query string, maxResults int) ([]BookResult, error) {
	if maxResults <= 0 {
		maxResults = 3
	}

	// url.QueryEscape handles spaces and special characters in the query.
	// Like Python's urllib.parse.quote().
	searchURL := fmt.Sprintf(
		"https://openlibrary.org/search.json?q=%s&limit=%d&fields=title,author_name,first_publish_year,subject,cover_i,number_of_pages_median",
		url.QueryEscape(query), maxResults,
	)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(searchURL)
	if err != nil {
		return nil, fmt.Errorf("open library request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("open library error (status %d): %s", resp.StatusCode, string(body))
	}

	var olResp openLibraryResponse
	if err := json.Unmarshal(body, &olResp); err != nil {
		return nil, fmt.Errorf("parsing response: %w", err)
	}

	var results []BookResult
	for _, doc := range olResp.Docs {
		book := BookResult{
			Title:          doc.Title,
			Authors:        doc.AuthorName,
			FirstPublished: doc.FirstPublishYear,
			PageCount:      doc.NumberOfPages,
		}

		// Limit subjects to first 5 to keep things concise.
		if len(doc.Subject) > 5 {
			book.Subjects = doc.Subject[:5]
		} else {
			book.Subjects = doc.Subject
		}

		// Build cover URL if available.
		if doc.CoverI > 0 {
			book.CoverURL = fmt.Sprintf("https://covers.openlibrary.org/b/id/%d-M.jpg", doc.CoverI)
		}

		results = append(results, book)
	}

	return results, nil
}

// FormatBookResults turns book results into a readable string for
// injection into the LLM context.
func FormatBookResults(books []BookResult) string {
	if len(books) == 0 {
		return "No books found."
	}

	var b strings.Builder
	for i, book := range books {
		fmt.Fprintf(&b, "%d. **%s**", i+1, book.Title)
		if len(book.Authors) > 0 {
			fmt.Fprintf(&b, " by %s", strings.Join(book.Authors, ", "))
		}
		if book.FirstPublished > 0 {
			fmt.Fprintf(&b, " (%d)", book.FirstPublished)
		}
		b.WriteString("\n")
		if book.PageCount > 0 {
			fmt.Fprintf(&b, "   Pages: %d\n", book.PageCount)
		}
		if len(book.Subjects) > 0 {
			fmt.Fprintf(&b, "   Subjects: %s\n", strings.Join(book.Subjects, ", "))
		}
	}
	return b.String()
}
