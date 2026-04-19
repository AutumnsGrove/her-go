// Package scenarios holds the concrete sim scenarios — one *.go file
// per flow. Each file registers its scenarios from init() so the test
// entry and the CLI both see them. Putting scenarios in a dedicated
// package means adding one is purely an insert, never an edit of
// existing code.
package scenarios

import (
	"context"
	"encoding/json"
	"fmt"

	"her/scheduler"
	"her/sim"
)

// scheduler_smoke is the hello-world scenario for the sim itself: a
// registered scheduler handler fires a Telegram send when its task
// comes due. If this scenario stops passing, the sim wiring is broken,
// not the feature it's exercising.

// smokeHandler is a minimal scheduler extension that sends a fixed
// message when it fires. No per-extension YAML — we register the task
// directly via SeedOneShot.
type smokeHandler struct{}

func (smokeHandler) Kind() string       { return "sim_smoke" }
func (smokeHandler) ConfigPath() string { return "" }
func (smokeHandler) Execute(_ context.Context, _ json.RawMessage, deps *scheduler.Deps) error {
	if deps.Send == nil {
		return fmt.Errorf("deps.Send is nil")
	}
	_, err := deps.Send(deps.ChatID, "smoke message from the scheduler")
	return err
}

func init() {
	// Register the handler exactly once, globally. The scheduler's
	// registry is package-level; tests should call withCleanRegistry()
	// inside the scheduler package, but for scenarios we want a real
	// persistent registration so the handler is present when
	// NewHarness builds a scheduler.
	scheduler.Register(smokeHandler{})

	sim.RegisterScenario(sim.Scenario{
		Name: "scheduler_smoke",
		Description: "A registered scheduler handler fires a Telegram " +
			"send when its task becomes due.",

		Setup: func(h *sim.Harness) error {
			// Seed one overdue task so the next tick dispatches it.
			return h.SeedOneShot("sim_smoke", json.RawMessage(`{}`))
		},

		Steps: []sim.Step{{
			Name: "tick the scheduler once",
			Do: func(h *sim.Harness) error {
				h.Scheduler.TickOnce(h.Ctx)
				return nil
			},
		}},

		Assertions: []sim.Assertion{
			{
				Name: "exactly one message was sent",
				Check: func(h *sim.Harness) error {
					sends := h.Transport.MessagesByKind(sim.EventSend)
					if len(sends) != 1 {
						return fmt.Errorf("got %d sends, want 1", len(sends))
					}
					return nil
				},
			},
			{
				Name: "the message text matches what the handler wrote",
				Check: func(h *sim.Harness) error {
					last := h.Transport.LastMessage()
					if last == nil {
						return fmt.Errorf("no last message")
					}
					want := "smoke message from the scheduler"
					if last.Text != want {
						return fmt.Errorf("text = %q, want %q", last.Text, want)
					}
					return nil
				},
			},
			{
				Name: "the message was delivered to the test chat",
				Check: func(h *sim.Harness) error {
					last := h.Transport.LastMessage()
					if last.ChatID != h.ChatID {
						return fmt.Errorf("chat = %d, want %d", last.ChatID, h.ChatID)
					}
					return nil
				},
			},
		},
	})
}
