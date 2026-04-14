// book_search is a skill that searches for books using the Open Library API.
//
// No API key needed — Open Library is free and open. Returns book titles,
// authors, publication years, page counts, subjects, and cover image URLs.
//
// Usage (via harness):
//
//	echo '{"query":"children of time"}' | ./bin/book_search
//
// Usage (manual testing):
//
//	go run main.go --query "adrian tchaikovsky" --limit 5
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"

	"skillkit"
)

// Args defines the parameters this skill accepts.
type Args struct {
	Query string `json:"query" flag:"query" desc:"Book search query"`
	Limit int    `json:"limit" flag:"limit" desc:"Max results" default:"3"`
}

// BookResult holds the key info about a book.
type BookResult struct {
	Title          string   `json:"title"`
	Authors        []string `json:"authors"`
	FirstPublished int      `json:"first_publish_year"`
	Subjects       []string `json:"subjects,omitempty"`
	PageCount      int      `json:"page_count,omitempty"`
	CoverURL       string   `json:"cover_url,omitempty"`
}

// openLibraryResponse is the raw JSON from the Open Library search API.
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

// Output is the structured result we return to the harness.
type Output struct {
	Query     string       `json:"query"`
	Results   []BookResult `json:"results"`
	Formatted string       `json:"formatted"`
}

func main() {
	var args Args
	skillkit.ParseArgs(&args)

	if args.Query == "" {
		skillkit.Error("query is required")
	}
	if args.Limit <= 0 {
		args.Limit = 3
	}

	skillkit.Logf("searching books: %s (limit %d)", args.Query, args.Limit)

	books, err := searchBooks(args.Query, args.Limit)
	if err != nil {
		skillkit.Error(fmt.Sprintf("book search failed: %s", err))
	}

	formatted := formatResults(books)

	skillkit.Output(Output{
		Query:     args.Query,
		Results:   books,
		Formatted: formatted,
	})
}

// searchBooks queries the Open Library search API.
func searchBooks(query string, maxResults int) ([]BookResult, error) {
	searchURL := fmt.Sprintf(
		"https://openlibrary.org/search.json?q=%s&limit=%d&fields=title,author_name,first_publish_year,subject,cover_i,number_of_pages_median",
		url.QueryEscape(query), maxResults,
	)

	client := skillkit.HTTPClient()
	resp, err := client.Get(searchURL)
	if err != nil {
		return nil, fmt.Errorf("open library request: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading response: %w", err)
	}

	if resp.StatusCode != 200 {
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

		// Limit subjects to first 5 to keep output concise.
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

// formatResults builds a human-readable summary of book search results.
func formatResults(books []BookResult) string {
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
