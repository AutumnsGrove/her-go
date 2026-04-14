// Package web_read implements the web_read tool — extracts clean text
// from a URL using the Tavily Extract API.
//
// Like web_search, results are accumulated in ctx.SearchContext so
// the reply tool automatically includes them as reference material.
package web_read

import (
	"encoding/json"

	"her/logger"
	"her/search"
	"her/tools"
)

var log = logger.WithPrefix("tools/web_read")

func init() {
	tools.Register("web_read", Handle)
}

// Handle extracts text content from a URL via Tavily and returns it.
// The content is also stored in ctx.SearchContext for the reply tool.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "error: " + err.Error()
	}
	if ctx.TavilyClient == nil {
		return "error: web read not configured (no Tavily API key in config)"
	}
	if args.URL == "" {
		return "error: url is required"
	}

	log.Infof("  web_read: %s", args.URL)

	resp, err := ctx.TavilyClient.Extract([]string{args.URL})
	if err != nil {
		log.Warn("web read failed", "url", args.URL, "err", err)
		return "error: " + err.Error()
	}

	formatted := search.FormatExtractResults(resp)

	// Accumulate in SearchContext alongside any search results from this turn.
	if ctx.SearchContext != "" {
		ctx.SearchContext += "\n\n"
	}
	ctx.SearchContext += formatted

	return formatted
}
