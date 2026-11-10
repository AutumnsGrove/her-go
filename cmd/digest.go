package cmd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

// This file turns a sim report (markdown, written by cmd/sim_gw.go's
// generateSimReport) into a small machine-readable digest, and compares two
// digests.
//
// Why: LLM output is not deterministic, so "did the migration change anything"
// can never be answered by diffing report text. The digest keeps only the
// structural facts that should be stable — tool sequence per turn, whether a
// turn reached `done`, costs, cache reads, provider routing, how many memories
// were saved — and the comparison applies the tolerances from
// docs/plans/PLAN-nova-migration.md §4.3.
//
// Baselines live in docs/plans/baselines/ as digests only (no prose, no
// secrets). See docs/plans/NOVA-MIGRATION-LOG.md for the run history.

// DigestTurn is one conversation turn, reduced to structure.
type DigestTurn struct {
	Turn        int      `json:"turn"`
	DurationS   float64  `json:"duration_s"`
	CostUSD     float64  `json:"cost_usd"`
	ToolCalls   int      `json:"tool_calls"`
	Tools       []string `json:"tools"` // tool names in order, across all agents in the turn
	CacheRead   int      `json:"cache_read_tokens"`
	CacheWrite  int      `json:"cache_write_tokens"`
	Providers   []string `json:"providers"`
	MemorySaves int      `json:"memory_saves"`
	Error       bool     `json:"error"`

	// ReachedDone reports whether the driver called `done`. A turn that exits
	// without it was cut short (max continuations, no tool calls, error) — the
	// "truncated turn" signal from §4.3, derived from behaviour rather than from
	// an exit-reason field the report does not print.
	ReachedDone bool `json:"reached_done"`
	// ReachedReply reports whether the driver called `reply`.
	ReachedReply bool `json:"reached_reply"`
}

// Digest is one sim run, reduced to structure.
type Digest struct {
	Suite        string            `json:"suite"`
	Date         string            `json:"date"`
	DurationS    string            `json:"duration"`
	CostUSD      float64           `json:"cost_usd"`
	Turns        int               `json:"turns"`
	TotalTools   int               `json:"total_tools"`
	ToolSet      map[string]int    `json:"tool_set"`
	PerTurn      []DigestTurn      `json:"per_turn"`
	MemoriesPost int               `json:"memories_post_run"`
	MoodsPost    int               `json:"moods_post_run"`
	MemoriesSaved int              `json:"memory_saves_total"`
	ErrorToolResults int           `json:"error_tool_results"`
	FallbackCalls int              `json:"fallback_calls"`
	CacheHitPct  float64           `json:"cache_hit_pct"`
	Models       map[string]string `json:"models"`
}

// Regular expressions for the report's stable lines. Each one is anchored to
// text generateSimReport writes in exactly one place; digest_test.go pins them
// against a real report excerpt so a format change fails a test instead of
// silently producing empty digests.
var (
	digestTitleRE   = regexp.MustCompile(`^# Simulation Report: (.+)$`)
	digestStatsRE   = regexp.MustCompile(`\*\*Duration:\*\* (\S+) \| \*\*Cost:\*\* \$([0-9.]+) \| \*\*Turns:\*\* (\d+)`)
	digestDateRE    = regexp.MustCompile(`\*\*Date:\*\* ([0-9: -]+) \|`)
	digestTurnRE    = regexp.MustCompile(`^### Turn (\d+) \*\((.*)\)\*`)
	digestMemoryRE  = regexp.MustCompile(`^> 🧩 \*\*memory:\*\* saved (\d+)`)
	digestTraceRE   = regexp.MustCompile(`^\[([^\]]+)\] \S+ (.+)$`)
	digestMemPostRE = regexp.MustCompile(`^\*\*(\d+) active memories:\*\*`)
	digestMoodPostRE = regexp.MustCompile(`^\*\*(\d+) mood entries`)
	digestFallbackRE = regexp.MustCompile(`^\*\*(\d+) of (\d+) calls`)
	digestHitRE      = regexp.MustCompile(`\*\*Cache hit rate\*\* \| \*\*([0-9.]+)%\*\*`)
	digestModelRowRE = regexp.MustCompile(`^\| (Chat|Driver|Memory|Mood|Classifier|Vision|Introspection|Dream|Embed) \| ([^|]+?) \|`)
)

// ExtractDigest parses a sim report into a Digest.
func ExtractDigest(report string) (Digest, error) {
	d := Digest{ToolSet: map[string]int{}, Models: map[string]string{}}

	lines := strings.Split(report, "\n")
	section := ""       // current "## " heading
	inTrace := false     // inside a ``` code block
	var turn *DigestTurn // turn being filled

	for _, raw := range lines {
		line := strings.TrimRight(raw, "\r")

		if m := digestTitleRE.FindStringSubmatch(line); m != nil {
			d.Suite = strings.TrimSpace(m[1])
			continue
		}
		if m := digestDateRE.FindStringSubmatch(line); m != nil && d.Date == "" {
			d.Date = strings.TrimSpace(m[1])
		}
		if m := digestStatsRE.FindStringSubmatch(line); m != nil {
			d.DurationS = m[1]
			d.CostUSD, _ = strconv.ParseFloat(m[2], 64)
			d.Turns, _ = strconv.Atoi(m[3])
			continue
		}
		if m := digestModelRowRE.FindStringSubmatch(line); m != nil && section == "" {
			d.Models[m[1]] = strings.TrimSpace(m[2])
			continue
		}
		if strings.HasPrefix(line, "## ") {
			section = strings.TrimPrefix(line, "## ")
			inTrace = false
			turn = nil
			continue
		}
		if strings.HasPrefix(line, "<details>") {
			inTrace = false
			continue
		}
		if strings.HasPrefix(line, "```") {
			inTrace = !inTrace
			continue
		}

		if m := digestTurnRE.FindStringSubmatch(line); m != nil {
			t := DigestTurn{Turn: atoiSafe(m[1])}
			parseTurnHeader(m[2], &t)
			d.PerTurn = append(d.PerTurn, t)
			turn = &d.PerTurn[len(d.PerTurn)-1]
			inTrace = false
			continue
		}

		if turn != nil {
			if m := digestMemoryRE.FindStringSubmatch(line); m != nil {
				turn.MemorySaves = atoiSafe(m[1])
				continue
			}
			if inTrace {
				if m := digestTraceRE.FindStringSubmatch(line); m != nil {
					source, name, isErr := parseTraceLine(m[1], m[2])
					if name != "" {
						turn.Tools = append(turn.Tools, name)
						d.ToolSet[name]++
						d.TotalTools++
						if isErr {
							d.ErrorToolResults++
						}
						// done/reply only count when the *driver* called them — the
						// memory/mood agents call done too, and that must not mask a
						// driver turn that was cut short.
						if source == driverSource {
							switch name {
							case "done":
								turn.ReachedDone = true
							case "reply":
								turn.ReachedReply = true
							}
						}
					}
				}
				continue
			}
		}

		switch section {
		case "Memories (post-run)":
			if m := digestMemPostRE.FindStringSubmatch(line); m != nil {
				d.MemoriesPost = atoiSafe(m[1])
			}
		case "Mood Entries":
			if m := digestMoodPostRE.FindStringSubmatch(line); m != nil {
				d.MoodsPost = atoiSafe(m[1])
			}
		case "Fallback Events":
			if m := digestFallbackRE.FindStringSubmatch(line); m != nil {
				d.FallbackCalls = atoiSafe(m[1])
			}
		case "Cache Efficiency":
			if m := digestHitRE.FindStringSubmatch(line); m != nil {
				d.CacheHitPct, _ = strconv.ParseFloat(m[1], 64)
			}
		}
	}

	for _, t := range d.PerTurn {
		d.MemoriesSaved += t.MemorySaves
	}

	if d.Suite == "" {
		return d, fmt.Errorf("not a sim report: no '# Simulation Report:' title found")
	}
	return d, nil
}

// parseTurnHeader fills duration/cost/tool count/cache/providers from the
// parenthesised part of a turn heading, e.g.
//
//	28.8s, $0.0047, 11 tools, cache ↓33728 ↑0, via Cohere
func parseTurnHeader(h string, t *DigestTurn) {
	parts := strings.Split(h, ", ")
	for _, p := range parts {
		p = strings.TrimSpace(p)
		switch {
		case strings.HasSuffix(p, "s") && strings.Contains(p, ".") && !strings.Contains(p, "$"):
			t.DurationS, _ = strconv.ParseFloat(strings.TrimSuffix(p, "s"), 64)
		case strings.HasPrefix(p, "$"):
			t.CostUSD, _ = strconv.ParseFloat(strings.TrimPrefix(p, "$"), 64)
		case strings.HasSuffix(p, " tools"):
			t.ToolCalls = atoiSafe(strings.TrimSuffix(p, " tools"))
		case strings.HasPrefix(p, "cache ↓"):
			// cache ↓33728 ↑0
			fields := strings.Split(strings.TrimPrefix(p, "cache "), " ")
			for _, f := range fields {
				if strings.HasPrefix(f, "↓") {
					t.CacheRead = atoiSafe(strings.TrimPrefix(f, "↓"))
				}
				if strings.HasPrefix(f, "↑") {
					t.CacheWrite = atoiSafe(strings.TrimPrefix(f, "↑"))
				}
			}
		case strings.HasPrefix(p, "via "):
			t.Providers = append(t.Providers, strings.Split(strings.TrimPrefix(p, "via "), "+")...)
		}
	}
}

// driverSource is the turn-phase name the driver agent runs under (see
// turn/registry.go — "main" registers at order 100). The background agents
// (memory, mood, introspection) run under their own names.
const driverSource = "main"

// parseTraceLine pulls the tool name and error flag out of one trace line
// (already split into its source and the rest).
//
//	create_schedule → Schedule #1 created: ...
//	save_memory → ERROR: rejected: ...
//	style→gate → no issues detected
//	think → {"thought": ...}
//
// Older reports (before the think trace line included its tool name) start the
// rest with the thought JSON instead: those are treated as "think".
func parseTraceLine(source, rest string) (src, name string, isErr bool) {
	src = source
	if i := strings.Index(rest, " → "); i >= 0 {
		name = strings.TrimSpace(rest[:i])
		isErr = strings.HasPrefix(strings.TrimSpace(rest[i+len(" → "):]), "ERROR:")
	} else {
		name = strings.TrimSpace(rest)
	}
	if strings.HasPrefix(name, "{") {
		name = "think"
	}
	return src, name, isErr
}

// atoiSafe is strconv.Atoi for data read out of a report: a malformed number
// becomes 0 instead of failing the whole digest.
func atoiSafe(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

// DigestCheck is one comparison result.
type DigestCheck struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// CompareDigests applies the §4.3 tolerances and returns the checks in a stable
// order. It compares structure, never prose.
func CompareDigests(base, next Digest) []DigestCheck {
	var checks []DigestCheck

	// 1. Every turn that completed in the baseline must complete in the new run.
	// "Completed" = the driver reached `done`. Turn counts may legitimately
	// differ (the suite is the same, so this is a hard failure).
	check := DigestCheck{Name: "turn count"}
	if base.Turns == next.Turns {
		check.OK = true
		check.Detail = fmt.Sprintf("%d turns both runs", base.Turns)
	} else {
		check.Detail = fmt.Sprintf("baseline %d turns, new %d turns", base.Turns, next.Turns)
	}
	checks = append(checks, check)

	// 2. Turn completion: no turn should stop reaching `done`.
	baseDone := countTrue(base, func(t DigestTurn) bool { return t.ReachedDone })
	nextDone := countTrue(next, func(t DigestTurn) bool { return t.ReachedDone })
	checks = append(checks, DigestCheck{
		Name:   "turns reaching done",
		OK:     nextDone >= baseDone,
		Detail: fmt.Sprintf("baseline %d/%d, new %d/%d", baseDone, len(base.PerTurn), nextDone, len(next.PerTurn)),
	})

	// 3. Tool vocabulary: no tool that the baseline exercised may disappear.
	var missing, added []string
	for name := range base.ToolSet {
		if _, ok := next.ToolSet[name]; !ok {
			missing = append(missing, name)
		}
	}
	for name := range next.ToolSet {
		if _, ok := base.ToolSet[name]; !ok {
			added = append(added, name)
		}
	}
	sort.Strings(missing)
	sort.Strings(added)
	check = DigestCheck{Name: "tool set"}
	switch {
	case len(missing) == 0:
		check.OK = true
		check.Detail = fmt.Sprintf("all %d baseline tools exercised", len(base.ToolSet))
		if len(added) > 0 {
			check.Detail += fmt.Sprintf("; new: %s", strings.Join(added, ", "))
		}
	default:
		check.Detail = fmt.Sprintf("missing from new run: %s", strings.Join(missing, ", "))
		if len(added) > 0 {
			check.Detail += fmt.Sprintf("; new: %s", strings.Join(added, ", "))
		}
	}
	checks = append(checks, check)

	// 4. Error-prefixed tool results should not increase.
	check = DigestCheck{Name: "error tool results"}
	check.OK = next.ErrorToolResults <= base.ErrorToolResults
	check.Detail = fmt.Sprintf("baseline %d, new %d", base.ErrorToolResults, next.ErrorToolResults)
	checks = append(checks, check)

	// 5. Memories saved within ±30% (absolute slack of 1 so tiny counts pass).
	check = DigestCheck{Name: "memories saved (±30%)"}
	check.OK = withinFraction(float64(base.MemoriesSaved), float64(next.MemoriesSaved), 0.30, 1)
	check.Detail = fmt.Sprintf("baseline %d, new %d", base.MemoriesSaved, next.MemoriesSaved)
	checks = append(checks, check)

	// 6. Cost within ±25% — the silent-regression detector (a prompt-cache drop
	// shows up here first).
	check = DigestCheck{Name: "cost (±25%)"}
	check.OK = withinFraction(base.CostUSD, next.CostUSD, 0.25, 0)
	check.Detail = fmt.Sprintf("baseline $%.4f, new $%.4f", base.CostUSD, next.CostUSD)
	checks = append(checks, check)

	// 7. No new fallback usage.
	check = DigestCheck{Name: "fallback calls"}
	check.OK = next.FallbackCalls <= base.FallbackCalls
	check.Detail = fmt.Sprintf("baseline %d, new %d", base.FallbackCalls, next.FallbackCalls)
	checks = append(checks, check)

	// 8. Cache read tokens must still be non-zero from turn 2 onward — the
	// prompt-cache-stability signal from delta D7.
	badCache := 0
	for _, t := range next.PerTurn {
		if t.Turn >= 2 && t.CacheRead == 0 {
			badCache++
		}
	}
	check = DigestCheck{Name: "cache reads from turn 2"}
	check.OK = badCache == 0
	check.Detail = fmt.Sprintf("%d turns with no cache read (hit rate %.1f%% vs baseline %.1f%%)",
		badCache, next.CacheHitPct, base.CacheHitPct)
	checks = append(checks, check)

	return checks
}

func countTrue(d Digest, pred func(DigestTurn) bool) int {
	n := 0
	for _, t := range d.PerTurn {
		if pred(t) {
			n++
		}
	}
	return n
}

// withinFraction reports whether next is within ±frac of base, with an absolute
// slack term so "0 vs 1 memory" is not a failure.
func withinFraction(base, next, frac, slack float64) bool {
	diff := next - base
	if diff < 0 {
		diff = -diff
	}
	if diff <= slack {
		return true
	}
	if base == 0 {
		return false
	}
	return diff/base <= frac
}

// --- CLI ---

var (
	digestOutFlag     string
	digestCompareFlag []string
)

var digestCmd = &cobra.Command{
	Use:   "digest [report.md ...]",
	Short: "Turn sim reports into structural digests, and compare digests",
	Long: `Extracts a stable, structural summary from a sim report — tool sequence per
turn, turn completion, cost, cache reads, provider routing, memory counts — and
writes it as JSON.

LLM output is nondeterministic, so comparisons run on the digest, not the prose.
Baselines for the Nova migration live in docs/plans/baselines/.

Examples:
  her digest sims/results/core-loop-and-dream-*.md --out docs/plans/baselines
  her digest --compare docs/plans/baselines/core-loop-a.json --compare docs/plans/baselines/core-loop-b.json`,
	RunE: runDigest,
}

func init() {
	digestCmd.Flags().StringVar(&digestOutFlag, "out", "", "directory to write <suite>-<timestamp>.json (default: print to stdout)")
	digestCmd.Flags().StringArrayVar(&digestCompareFlag, "compare", nil, "compare two digest JSON files: pass --compare <baseline> --compare <new>")
	rootCmd.AddCommand(digestCmd)
}

func runDigest(cmd *cobra.Command, args []string) error {
	if len(digestCompareFlag) > 0 {
		if len(digestCompareFlag) != 2 {
			return fmt.Errorf("--compare takes exactly two files: --compare <baseline.json> --compare <new.json>")
		}
		base, err := loadDigest(digestCompareFlag[0])
		if err != nil {
			return err
		}
		next, err := loadDigest(digestCompareFlag[1])
		if err != nil {
			return err
		}
		fmt.Printf("baseline: %s (%s, %d turns, $%.4f)\n", base.Suite, base.Date, base.Turns, base.CostUSD)
		fmt.Printf("new:      %s (%s, %d turns, $%.4f)\n\n", next.Suite, next.Date, next.Turns, next.CostUSD)

		failed := 0
		for _, c := range CompareDigests(base, next) {
			status := "PASS"
			if !c.OK {
				status = "FAIL"
				failed++
			}
			fmt.Printf("  %-4s %-26s %s\n", status, c.Name, c.Detail)
		}
		if failed > 0 {
			return fmt.Errorf("%d digest check(s) failed", failed)
		}
		return nil
	}

	if len(args) == 0 {
		return fmt.Errorf("give at least one report path, or use --compare")
	}
	for _, path := range args {
		data, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("reading %s: %w", path, err)
		}
		d, err := ExtractDigest(string(data))
		if err != nil {
			return fmt.Errorf("%s: %w", path, err)
		}
		encoded, err := json.MarshalIndent(d, "", "  ")
		if err != nil {
			return fmt.Errorf("encoding digest: %w", err)
		}
		if digestOutFlag == "" {
			fmt.Println(string(encoded))
			continue
		}
		if err := os.MkdirAll(digestOutFlag, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", digestOutFlag, err)
		}
		slug := slugifySuite(d.Suite)
		dest := filepath.Join(digestOutFlag, fmt.Sprintf("%s-%s.json", slug, slugifySuite(d.Date)))
		if err := os.WriteFile(dest, append(encoded, '\n'), 0o644); err != nil {
			return fmt.Errorf("writing %s: %w", dest, err)
		}
		fmt.Printf("%s  (%d turns, $%.4f, %d tool calls)\n", dest, d.Turns, d.CostUSD, d.TotalTools)
	}
	return nil
}

func loadDigest(path string) (Digest, error) {
	var d Digest
	data, err := os.ReadFile(path)
	if err != nil {
		return d, fmt.Errorf("reading %s: %w", path, err)
	}
	if err := json.Unmarshal(data, &d); err != nil {
		return d, fmt.Errorf("parsing %s: %w", path, err)
	}
	return d, nil
}

// slugifySuite makes a filesystem-safe name: lowercase, spaces and punctuation
// collapsed to single dashes.
func slugifySuite(s string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(s) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
