// Package web_search implements the web_search tool — searches the web
// via the Tavily API and returns ranked snippets plus a synthesized answer.
//
// Results are accumulated in ctx.SearchContext so that when the agent
// calls reply, the search findings are automatically included as
// reference material for the chat model. This is the same pattern used
// by view_image (which stores image descriptions in SearchContext).
package web_search

import (
	"encoding/json"

	"her/logger"
	"her/search"
	"her/tools"
)

var log = logger.WithPrefix("tools/web_search")

func init() {
	tools.Register("web_search", Handle)
}

// Handle runs a web search via Tavily and returns formatted results.
// The results are also stored in ctx.SearchContext for the reply tool.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "error: " + err.Error()
	}
	if ctx.TavilyClient == nil {
		return "error: web search not configured (no Tavily API key in config)"
	}
	if args.Limit <= 0 {
		args.Limit = 5
	}

	log.Infof("  web_search: %q (limit %d)", args.Query, args.Limit)

	resp, err := ctx.TavilyClient.Search(args.Query, args.Limit)
	if err != nil {
		log.Warn("web search failed", "query", args.Query, "err", err)
		return "error: " + err.Error()
	}

	formatted := search.FormatSearchResults(resp)

	// Accumulate results in SearchContext. The reply tool reads this and
	// injects it as reference material for the chat model, so search
	// context carries across multiple searches in the same turn.
	if ctx.SearchContext != "" {
		ctx.SearchContext += "\n\n"
	}
	ctx.SearchContext += formatted

	return formatted
}
