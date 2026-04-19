# sim — headless end-to-end simulator

Run the full bot stack — scheduler, mood agent, memory store, Telegram
wire layer, LLM calls — without ever talking to Telegram or OpenRouter.
Every Telegram interaction goes into an in-memory event log you can
walk to assert on what the user would have seen.

## Running

```bash
# Under `go test` — the usual CI path
go test ./sim/...
go test -v ./sim/...          # narrate every step + assertion

# Standalone CLI — good for eyeballing a scenario manually
go run ./sim/cmd/simtest                        # list scenarios
go run ./sim/cmd/simtest -scenario foo          # run one
go run ./sim/cmd/simtest -all                   # run everything
```

No API keys, no network, no Telegram tokens needed. Scenarios
deterministic.

## Anatomy of a Harness

`sim.NewHarness(t, opts)` gives you:

- `h.Store` — real SQLite in a temp dir (every table your bot uses)
- `h.Transport` — a `FakeTransport` that records every Send / Edit /
  SendPNG call; `Transport.Events()` returns the transcript
- `h.Clock` — a `FakeClock`. Move time with `h.Clock.Advance(1*time.Hour)`
- `h.LLM` — a `FakeLLM` (httptest server) that replies with scripted
  completions. `h.LLM.Script("I'm exhausted", "{valence: 2, labels: [stressed]}")`
- `h.Scheduler` — a real `scheduler.Scheduler` wired to the rest. Call
  `h.Scheduler.TickOnce(h.Ctx)` to dispatch due tasks synchronously
- `h.ChatID` — the owner chat ID (42 by default)

## Writing a Scenario

Drop a new file in `sim/scenarios/`. Pattern:

```go
package scenarios

import (
    "encoding/json"
    "fmt"
    "time"

    "her/sim"
)

func init() {
    sim.RegisterScenario(sim.Scenario{
        Name:        "mood_high_confidence_auto_log",
        Description: "High-confidence inferred mood is auto-logged without UI.",

        Setup: func(h *sim.Harness) error {
            h.LLM.Script("I'm exhausted", `{"valence":2,"labels":["Stressed"],"confidence":0.9}`)
            return nil
        },

        Steps: []sim.Step{{
            Name: "user sends a message with a strong affect signal",
            Do: func(h *sim.Harness) error {
                // Call whatever entry point drives the mood agent
                // directly (no Telegram round-trip required).
                return triggerMoodAgent(h, "I'm absolutely exhausted today")
            },
        }},

        Assertions: []sim.Assertion{
            {
                Name: "one mood entry was saved with source=inferred",
                Check: func(h *sim.Harness) error {
                    // Query h.Store directly.
                    return nil // TODO
                },
            },
            {
                Name: "no Telegram proposal was sent",
                Check: func(h *sim.Harness) error {
                    if sends := h.Transport.MessagesByKind(sim.EventSend); len(sends) != 0 {
                        return fmt.Errorf("expected no sends, got %d", len(sends))
                    }
                    return nil
                },
            },
        },
    })
}
```

The scenario is picked up automatically by both `go test ./sim/...`
and the CLI via the registry. No wiring edits needed when you add
scenarios.

## Scenarios we intend to build as each mood piece lands

| Scenario | What it proves |
|----------|----------------|
| `scheduler_smoke` ✓ | Scheduler + FakeTransport wiring works end-to-end. |
| `mood_inferred_high_auto_log` | Confidence ≥ 0.75 writes `source=inferred` directly, no proposal. |
| `mood_inferred_medium_proposal` | Confidence 0.40–0.75 emits a Telegram proposal with inline buttons. |
| `mood_inferred_low_drops` | Confidence < 0.40 writes nothing, sends nothing. |
| `mood_dedup_skips_near_duplicate` | A near-identical inferred mood within the dedup window is skipped. |
| `mood_proposal_expiry_sweeper` | Untapped proposal's buttons get edited to "expired" after N minutes. |
| `mood_proposal_user_confirms` | Tapping "Log it" on a proposal writes `source=confirmed`. |
| `mood_daily_rollup_algorithmic` | 21:00 cron fires, algorithmic draft sent with inline buttons. |
| `mood_daily_rollup_auto_log_next_morning` | Unresponded draft auto-logs at 08:00 as `source=inferred, kind=daily`. |
| `mood_manual_wizard_full_flow` | 4-step `/mood` wizard edits message in place through each step. |
| `mood_graph_png_reply` | `/mood week` emits a `SendPNG` with non-empty bytes. |

Each of these lands with the corresponding feature.

## Guarantees the sim gives you

- Deterministic: no wall clock, no network, no random.
- Full fidelity: real Store, real Scheduler, real scrubber, real embed
  code (when embedDim > 0). The ONLY fakes are Telegram, LLM, and clock.
- Runnable anywhere: no external services, no test doubles beyond the
  trust boundary.

## What the sim can't tell you

- Whether the real LLM will actually produce the structured output your
  handler expects. That's what style gates, the classifier, and
  production logging catch.
- Whether Telegram renders your inline keyboards the way you expect.
  That's a manual eyeball, once.
- Performance under real load. Sim is correctness-only.
