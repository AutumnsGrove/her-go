package search

// books.go — Open Library book search.
//
// Open Library is a free API backed by the Internet Archive. No API key,
// no rate limits for reasonable use. We hit the /search.json endpoint
// and return a handful of fields useful for grounding book conversations:
// title, authors, first-published year, subjects, page count, cover URL.
//
// This lives in the search package alongside Tavily because it's the
// same shape of problem — structured lookup against an external API —
// even though books use their own client (no shared auth with Tavily).

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// BookResult holds the fields we show from a single Open Library hit.
type BookResult struct {
	Title          string
	Authors        []string
	FirstPublished int
	Subjects       []string
	PageCount      int
	CoverURL       string
}

// openLibraryResponse mirrors the JSON shape returned by Open Library's
// /search.json endpoint. We only unmarshal the fields we use — the raw
// response has ~60 more we don't care about.
type openLibraryResponse struct {
	Docs []struct {
		Title            string   `json:"title"`
		AuthorName       []string `json:"author_name"`
		FirstPublishYear int      `json:"first_publish_year"`
		Subject          []string `json:"subject"`
		CoverI           int      `json:"cover_i"`
		NumberOfPages    int      `json:"number_of_pages_median"`
	} `json:"docs"`
}

// SearchBooks queries Open Library and returns up to maxResults hits.
// maxResults <= 0 defaults to 3; we also cap at 10 to keep tool output
// readable. If nothing matches, returns an empty slice (not an error).
func SearchBooks(query string, maxResults int) ([]BookResult, error) {
	if maxResults <= 0 {
		maxResults = 3
	}
	if maxResults > 10 {
		maxResults = 10
	}

	// The "fields" parameter asks Open Library to only return the
	// columns we use, shrinking the response from ~100KB to ~5KB.
	searchURL := fmt.Sprintf(
		"https://openlibrary.org/search.json?q=%s&limit=%d"+
			"&fields=title,author_name,first_publish_year,subject,cover_i,number_of_pages_median",
		url.QueryEscape(query), maxResults,
	)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(searchURL)
	if err != nil {
		return nil, fmt.Errorf("open library request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("open library returned %d: %s", resp.StatusCode, string(body))
	}

	var olResp openLibraryResponse
	if err := json.NewDecoder(resp.Body).Decode(&olResp); err != nil {
		return nil, fmt.Errorf("parsing open library response: %w", err)
	}

	results := make([]BookResult, 0, len(olResp.Docs))
	for _, doc := range olResp.Docs {
		book := BookResult{
			Title:          doc.Title,
			Authors:        doc.AuthorName,
			FirstPublished: doc.FirstPublishYear,
			PageCount:      doc.NumberOfPages,
		}
		// Cap subject tags — Open Library can return hundreds for popular books.
		if len(doc.Subject) > 5 {
			book.Subjects = doc.Subject[:5]
		} else {
			book.Subjects = doc.Subject
		}
		if doc.CoverI > 0 {
			book.CoverURL = fmt.Sprintf("https://covers.openlibrary.org/b/id/%d-M.jpg", doc.CoverI)
		}
		results = append(results, book)
	}

	return results, nil
}

// FormatBookResults renders a slice of BookResult into a readable string
// for the tool to return to the agent. Same shape as FormatSearchResults
// (Tavily) — numbered list, one entry per book.
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
