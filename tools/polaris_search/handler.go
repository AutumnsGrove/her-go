// Package polaris_search implements the polaris_search tool — delegates
// research to Polaris (a separate self-hosted agent), which runs its own
// multi-step web search + synthesis loop and returns one finished,
// cited answer instead of raw snippets.
//
// Like web_search/web_read, the answer is accumulated in ctx.SearchContext
// so it's automatically included as reference material when reply runs.
package polaris_search

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"her/logger"
	"her/memory"
	"her/tools"
)

var log = logger.WithPrefix("tools/polaris_search")

func init() {
	tools.Register("polaris_search", Handle)
}

// askResponse mirrors Polaris's POST /api/ask response shape (see
// polaris/gateway/ask.go AskResponse) — only the fields this tool uses.
type askResponse struct {
	Answer    string `json:"answer"`
	Citations []struct {
		Title string `json:"title"`
		URL   string `json:"url"`
	} `json:"citations"`
	// CostUSD is what Polaris itself spent (its own LLM + reasoning calls)
	// answering this query — real money already spent by a service outside
	// her-go's own token accounting, so it has to be folded in explicitly
	// (see the SaveMetric call below) rather than showing up for free.
	CostUSD float64 `json:"cost_usd"`
}

// httpClient allows for a generous timeout: Polaris's own runaway-search
// steering (interval check-ins + stale-streak detection) keeps a single
// turn to a handful of tool calls in practice, but a genuinely hard
// multi-part question can still take a couple of minutes end to end.
var httpClient = &http.Client{Timeout: 180 * time.Second}

func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "error: " + err.Error()
	}
	if args.Query == "" {
		return "error: query is required"
	}
	if ctx.Cfg == nil || ctx.Cfg.Polaris.BaseURL == "" {
		return "error: polaris not configured (set polaris.base_url in config.yaml)"
	}

	log.Infof("  polaris_search: %q", args.Query)

	reqBody, err := json.Marshal(map[string]string{
		"content": args.Query,
		"source":  "her-go",
	})
	if err != nil {
		return "error: " + err.Error()
	}

	req, err := http.NewRequest("POST", ctx.Cfg.Polaris.BaseURL+"/api/ask", bytes.NewReader(reqBody))
	if err != nil {
		return "error: creating request: " + err.Error()
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		log.Warn("polaris request failed", "query", args.Query, "err", err)
		return "error: request failed: " + err.Error()
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "error: reading response: " + err.Error()
	}

	if resp.StatusCode != http.StatusOK {
		log.Warn("polaris returned an error status", "query", args.Query, "status", resp.StatusCode, "body", string(body))
		return fmt.Sprintf("error: polaris returned %d: %s", resp.StatusCode, string(body))
	}

	var parsed askResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "error: parsing response: " + err.Error()
	}

	log.Infof("  polaris_search: got %d-char answer, %d citations, cost $%.6f", len(parsed.Answer), len(parsed.Citations), parsed.CostUSD)

	// Fold Polaris's own spend into this turn's cost tracking — without
	// this, delegating to Polaris would look free in every cost total,
	// dashboard, and sim report, when real money (OpenRouter calls Polaris
	// made on our behalf) was actually spent. Two separate accumulators,
	// both needed: SaveMetric persists it for later queries (GetStats,
	// /usage, CostForMessage); ToolAPICost makes it count toward *this
	// turn's* live total (see agent.go/worker.go's RunResult/WorkerResult
	// CostUSD) — same pattern as view_image's vision-API cost, but that
	// tool only does the SaveMetric half, not the live-total half.
	if parsed.CostUSD > 0 {
		ctx.ToolAPICost += parsed.CostUSD
		if ctx.Store != nil && ctx.TriggerMsgID > 0 {
			if err := ctx.Store.SaveMetric(memory.MetricInput{
				Model:     "polaris",
				CostUSD:   parsed.CostUSD,
				MessageID: ctx.TriggerMsgID,
				AgentRole: memory.RolePolaris,
			}); err != nil {
				log.Error("saving polaris_search cost metric", "err", err)
			}
		}
	}

	formatted := parsed.Answer
	if len(parsed.Citations) > 0 {
		formatted += "\n\nSources:\n"
		for _, c := range parsed.Citations {
			formatted += fmt.Sprintf("- %s (%s)\n", c.Title, c.URL)
		}
	}

	// Accumulate in SearchContext alongside any web_search/web_read
	// results from this turn, same pattern as those tools.
	if ctx.SearchContext != "" {
		ctx.SearchContext += "\n\n"
	}
	ctx.SearchContext += formatted

	return formatted
}
