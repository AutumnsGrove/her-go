// Package search_books implements the search_books tool — queries
// Open Library for books matching a title, author, topic, or ISBN.
//
// Like web_search, results are accumulated into ctx.SearchContext so
// the reply tool automatically injects them as reference material for
// the chat model. This keeps the two "lookup" tools consistent — if
// the agent calls search_books twice in a turn, both result sets end
// up in the chat prompt.
package search_books

import (
	"encoding/json"
	"fmt"

	"her/logger"
	"her/search"
	"her/tools"
)

var log = logger.WithPrefix("tools/search_books")

func init() {
	tools.Register("search_books", Handle)
}

// Handle runs a book search and returns formatted results.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "error: " + err.Error()
	}
	if args.Query == "" {
		return "error: query is required"
	}
	if args.Limit <= 0 {
		args.Limit = 3
	}

	log.Infof("  search_books: %q (limit %d)", args.Query, args.Limit)

	books, err := search.SearchBooks(args.Query, args.Limit)
	if err != nil {
		log.Warn("book search failed", "query", args.Query, "err", err)
		return "error: " + err.Error()
	}

	formatted := search.FormatBookResults(books)

	// Accumulate in SearchContext. The reply tool reads this and passes
	// it to the chat model as reference material — same pattern web_search
	// uses, so books and web results compose cleanly when the agent uses
	// both in one turn.
	if ctx.SearchContext != "" {
		ctx.SearchContext += "\n\n"
	}
	ctx.SearchContext += "## Book search results\n\n" + formatted

	// Return a shorter summary for the agent to read. The full formatted
	// block goes to the chat model via SearchContext; the agent just
	// needs to know what came back.
	if len(books) == 0 {
		return fmt.Sprintf("No books found for %q.", args.Query)
	}
	return fmt.Sprintf("Found %d books for %q. Results available in chat context.", len(books), args.Query)
}
