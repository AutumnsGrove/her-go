package cmd

import (
	"strings"
	"testing"
)

// fixtureReport is a hand-trimmed excerpt of a real report written by
// generateSimReport. It is the contract between the report writer and the
// digest extractor: if either side changes shape, this test fails instead of
// the digests quietly going empty.
const fixtureReport = `# Simulation Report: Schedule CRUD — create, list, update, delete recurring tasks

> Tests the full scheduling tool lifecycle.

**Date:** 2026-11-10 10:24 | **Duration:** 5m15s | **Cost:** $0.0376 | **Turns:** 2

| Role | Model | Provider |
|------|-------|----------|
| Chat | xiaomi/mimo-v2.6-pro | (auto) |
| Driver | xiaomi/mimo-v2.6-flash | (auto) |
| Embed | perplexity/pplx-embed-v1-0.6b | (auto) |

## Conversation

### Turn 1 *(28.8s, $0.0047, 11 tools, cache ↓33728 ↑0, via Cohere)*
**Autumn:** hey, can you set up a daily briefing?

**Mira:** Nothing actually got set up on my end.

> 🧩 **memory:** saved 2
> - saved user memory ID=1: Autumn follows news about Go.

> 🎭 **mood:** dropped_no_signal

<details><summary>Tool trace</summary>

` + "```" + `
[main] 🧰 use_tools → loaded: create_schedule, list_schedules
[main] 🔧 create_schedule → Schedule #1 created: "6183a9-Daily"
[main] 🧠 think → {"thought": "I should confirm the schedule exists"}
[main] 💬 reply → ok
[main] ✅ done → tool call complete, turn complete
[main] 🔧 style→gate → no issues detected
[memory] 🔍 recall_memories → ERROR: error: provide either id, slug, or query
[memory] 💾 save_memory → saved user memory ID=1: Autumn follows news about Go.
` + "```" + `
</details>

---

### Turn 2 *(38.3s, $0.0064, 3 tools, via Makora)*
**Autumn:** what schedules do I have?

**Mira:** You have one.

### Turn 3 *(ERROR)*
**Autumn:** boom

**Error:** context deadline exceeded

---

## Memories (post-run)

**3 active memories:**

1. Autumn follows news about Go.

## Mood Entries

**1 mood entries (latest first):**

## Fallback Events

**2 of 50 calls (4%) used fallback models** — $0.0010 in fallback costs

## Cache Efficiency

| Metric | Value |
|--------|-------|
| Total prompt tokens | 726749 |
| **Cache hit rate** | **72.5%** |
| Total cost | $0.0372 |
`

func TestExtractDigest(t *testing.T) {
	d, err := ExtractDigest(fixtureReport)
	if err != nil {
		t.Fatalf("ExtractDigest: %v", err)
	}

	if d.Suite != "Schedule CRUD — create, list, update, delete recurring tasks" {
		t.Errorf("Suite = %q", d.Suite)
	}
	if d.Date != "2026-11-10 10:24" {
		t.Errorf("Date = %q", d.Date)
	}
	if d.DurationS != "5m15s" {
		t.Errorf("DurationS = %q", d.DurationS)
	}
	if d.CostUSD != 0.0376 {
		t.Errorf("CostUSD = %v, want 0.0376", d.CostUSD)
	}
	if d.Turns != 2 {
		t.Errorf("Turns = %d, want 2 (the header count; turn 3 is an error turn)", d.Turns)
	}
	if d.Models["Driver"] != "xiaomi/mimo-v2.6-flash" {
		t.Errorf("Models[Driver] = %q", d.Models["Driver"])
	}

	// Turn 1: 8 trace lines, incl. the think line that carries its tool name.
	wantTools := []string{"use_tools", "create_schedule", "think", "reply", "done", "style→gate", "recall_memories", "save_memory"}
	t1 := d.PerTurn[0]
	if strings.Join(t1.Tools, ",") != strings.Join(wantTools, ",") {
		t.Errorf("turn 1 tools = %v, want %v", t1.Tools, wantTools)
	}
	if t1.DurationS != 28.8 || t1.CostUSD != 0.0047 || t1.ToolCalls != 11 {
		t.Errorf("turn 1 header: dur=%v cost=%v calls=%v", t1.DurationS, t1.CostUSD, t1.ToolCalls)
	}
	if t1.CacheRead != 33728 || t1.CacheWrite != 0 {
		t.Errorf("turn 1 cache: read=%d write=%d", t1.CacheRead, t1.CacheWrite)
	}
	if len(t1.Providers) != 1 || t1.Providers[0] != "Cohere" {
		t.Errorf("turn 1 providers = %v", t1.Providers)
	}
	if t1.MemorySaves != 2 {
		t.Errorf("turn 1 memory saves = %d, want 2", t1.MemorySaves)
	}
	if !t1.ReachedDone || !t1.ReachedReply {
		t.Errorf("turn 1 reached done=%v reply=%v, want both true", t1.ReachedDone, t1.ReachedReply)
	}

	// Turn 2 called no tools at all — the truncated-turn signal.
	t2 := d.PerTurn[1]
	if len(t2.Tools) != 0 || t2.ReachedDone || t2.ReachedReply {
		t.Errorf("turn 2 tools = %v done=%v reply=%v, want empty/false", t2.Tools, t2.ReachedDone, t2.ReachedReply)
	}

	if d.ErrorToolResults != 1 {
		t.Errorf("ErrorToolResults = %d, want 1", d.ErrorToolResults)
	}
	if d.MemoriesSaved != 2 {
		t.Errorf("MemoriesSaved = %d, want 2", d.MemoriesSaved)
	}
	if d.MemoriesPost != 3 {
		t.Errorf("MemoriesPost = %d, want 3", d.MemoriesPost)
	}
	if d.MoodsPost != 1 {
		t.Errorf("MoodsPost = %d, want 1", d.MoodsPost)
	}
	if d.FallbackCalls != 2 {
		t.Errorf("FallbackCalls = %d, want 2", d.FallbackCalls)
	}
	if d.CacheHitPct != 72.5 {
		t.Errorf("CacheHitPct = %v, want 72.5", d.CacheHitPct)
	}

	// The tool set aggregates across turns.
	if d.ToolSet["create_schedule"] != 1 || d.ToolSet["recall_memories"] != 1 {
		t.Errorf("ToolSet = %v", d.ToolSet)
	}
}

// TestExtractDigest_MemoryDoneDoesNotCountAsDriverDone is the regression test
// for the "memory agent also calls done" trap: a driver turn that never calls
// done must stay visible as truncated even when the memory agent finishes.
func TestExtractDigest_MemoryDoneDoesNotCountAsDriverDone(t *testing.T) {
	report := `# Simulation Report: X

**Date:** 2026-11-10 10:00 | **Duration:** 10s | **Cost:** $0.0010 | **Turns:** 1

## Conversation

### Turn 1 *(5.0s, $0.0010, 2 tools)*
**Autumn:** hi

**Mira:** hello

<details><summary>Tool trace</summary>

` + "```" + `
[main] 🔧 reply → ok
[memory] ✅ done → tool call complete, turn complete
` + "```" + `
</details>
`
	d, err := ExtractDigest(report)
	if err != nil {
		t.Fatalf("ExtractDigest: %v", err)
	}
	if d.PerTurn[0].ReachedDone {
		t.Error("memory agent's done must not mark the driver turn as complete")
	}
	if !d.PerTurn[0].ReachedReply {
		t.Error("driver reply should be recorded")
	}
}

func TestExtractDigest_RejectsNonReport(t *testing.T) {
	if _, err := ExtractDigest("# Some other markdown\n\nhello\n"); err == nil {
		t.Fatal("expected an error for a non-report input")
	}
}

func TestCompareDigests(t *testing.T) {
	base := Digest{
		Turns: 2, CostUSD: 0.04, MemoriesSaved: 4, FallbackCalls: 0, CacheHitPct: 70,
		ToolSet: map[string]int{"reply": 2, "done": 2, "search": 1},
		PerTurn: []DigestTurn{
			{Turn: 1, ReachedDone: true, ReachedReply: true},
			{Turn: 2, ReachedDone: true, ReachedReply: true, CacheRead: 100},
		},
	}

	t.Run("identical", func(t *testing.T) {
		for _, c := range CompareDigests(base, base) {
			if !c.OK {
				t.Errorf("%s should pass: %s", c.Name, c.Detail)
			}
		}
	})

	t.Run("cost blowout fails", func(t *testing.T) {
		next := base
		next.CostUSD = 0.06 // +50%, over the +25% ceiling
		if checkOK(CompareDigests(base, next), "cost not above +25%") {
			t.Error("a 50% cost increase should fail the cost check")
		}
	})

	t.Run("cost within band passes", func(t *testing.T) {
		next := base
		next.CostUSD = 0.048 // +20%
		if !checkOK(CompareDigests(base, next), "cost not above +25%") {
			t.Error("a 20% cost increase should pass")
		}
	})

	t.Run("cheaper run passes", func(t *testing.T) {
		next := base
		next.CostUSD = 0.02 // -50%: spending less is never a regression
		if !checkOK(CompareDigests(base, next), "cost not above +25%") {
			t.Error("a run that costs half as much should pass")
		}
	})

	t.Run("missing tool fails", func(t *testing.T) {
		next := base
		next.ToolSet = map[string]int{"reply": 2, "done": 2}
		if checkOK(CompareDigests(base, next), "tool set") {
			t.Error("a dropped tool should fail the tool-set check")
		}
	})

	t.Run("truncated turn fails", func(t *testing.T) {
		next := base
		next.PerTurn = []DigestTurn{
			{Turn: 1, ReachedDone: true, ReachedReply: true},
			{Turn: 2, ReachedReply: true, CacheRead: 100},
		}
		if checkOK(CompareDigests(base, next), "turns reaching done") {
			t.Error("a turn that stopped reaching done should fail")
		}
	})

	t.Run("cache drop fails", func(t *testing.T) {
		next := base
		next.PerTurn = []DigestTurn{
			{Turn: 1, ReachedDone: true, ReachedReply: true},
			{Turn: 2, ReachedDone: true, ReachedReply: true, CacheRead: 0},
		}
		if checkOK(CompareDigests(base, next), "cache reads from turn 2") {
			t.Error("no cache read on turn 2 should fail")
		}
	})

	t.Run("memory count within slack", func(t *testing.T) {
		next := base
		next.MemoriesSaved = 5 // +25%
		if !checkOK(CompareDigests(base, next), "memories saved (±30%)") {
			t.Error("a 25% memory-count change should pass")
		}
	})
}

func checkOK(checks []DigestCheck, name string) bool {
	for _, c := range checks {
		if c.Name == name {
			return c.OK
		}
	}
	return false
}

func TestSlugifySuite(t *testing.T) {
	cases := map[string]string{
		"Schedule CRUD — create, list, update, delete recurring tasks": "schedule-crud-create-list-update-delete-recurring-tasks",
		"2026-11-10 10:24":    "2026-11-10-10-24",
		"  weird   spacing  ": "weird-spacing",
	}
	for in, want := range cases {
		if got := slugifySuite(in); got != want {
			t.Errorf("slugifySuite(%q) = %q, want %q", in, got, want)
		}
	}
}
