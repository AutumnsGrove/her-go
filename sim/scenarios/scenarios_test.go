package scenarios

import (
	"testing"

	"her/sim"
)

// TestAllScenarios is the one entry point that runs every registered
// scenario as a subtest. Adding a new scenario file automatically
// grows this test — no wiring edits needed.
//
// Each scenario gets a fresh Harness, so state never leaks between
// scenarios. If one scenario fails its assertions, others still run.
func TestAllScenarios(t *testing.T) {
	for _, s := range sim.AllScenarios() {
		s := s
		t.Run(s.Name, func(t *testing.T) {
			h := sim.NewHarness(t, s.HarnessOptions)
			if err := s.Run(h); err != nil {
				t.Fatalf("%s: %v", s.Name, err)
			}
		})
	}
}
