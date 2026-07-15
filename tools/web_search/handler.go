// Package web_search implements the web_search tool — searches the web
// via SearXNG (if configured) or Tavily (fallback).
//
// SearXNG is preferred when available because it's free, self-hosted, and
// has no rate limits. Tavily is used as a fallback when SearXNG isn't
// configured.
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

// Handle runs a web search and returns formatted results.
// Prefers SearXNG (if configured), falls back to Tavily.
// The results are also stored in ctx.SearchContext for the reply tool.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "error: " + err.Error()
	}
	if args.Limit <= 0 {
		args.Limit = 5
	}

	log.Infof("  web_search: %q (limit %d)", args.Query, args.Limit)

	var resp *search.SearchResponse
	var err error
	var backend string

	// Try SearXNG first (free, local, no rate limits)
	if ctx.SearXNGClient != nil {
		backend = "searxng"
		resp, err = ctx.SearXNGClient.Search(args.Query, args.Limit)
	} else if ctx.TavilyClient != nil {
		// Fall back to Tavily
		backend = "tavily"
		resp, err = ctx.TavilyClient.Search(args.Query, args.Limit)
	} else {
		return "error: web search not configured (no SearXNG or Tavily in config)"
	}

	if err != nil {
		log.Warn("web search failed", "backend", backend, "query", args.Query, "err", err)
		return "error: " + err.Error()
	}

	log.Infof("  web_search: used %s, found %d results", backend, len(resp.Results))

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
