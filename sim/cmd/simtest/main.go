// simtest runs sim scenarios from the command line.
//
//	go run ./sim/cmd/simtest                    # list all scenarios
//	go run ./sim/cmd/simtest -scenario foo      # run one
//	go run ./sim/cmd/simtest -all               # run everything, narrated
//
// This CLI is a convenience for manual sanity checks. The scenarios are
// the same ones that run under `go test ./sim/...`. Both paths use
// sim.AllScenarios() as the source of truth.
package main

import (
	"flag"
	"fmt"
	"os"

	"her/sim"

	// Blank-import the scenarios package so its init() runs and
	// registers every scenario in the global registry.
	_ "her/sim/scenarios"
)

// cliT satisfies sim.TestingT without pulling in testing.TB (which has
// unexported methods and can't be implemented outside the testing
// package). Only four methods are needed; everything else the testing
// package provides is scenario-internal.
//
// Fatalf and the cleanup set are the important ones:
//   - Fatalf is called by the harness when core setup (SQLite, scheduler)
//     fails; we exit 1 so CI / shell pipelines see the failure.
//   - Cleanup funcs are run in LIFO order when the CLI exits.
type cliT struct {
	cleanups []func()
}

func (c *cliT) Helper() {}

func (c *cliT) Fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	c.runCleanups()
	os.Exit(1)
}

func (c *cliT) Cleanup(fn func()) {
	c.cleanups = append(c.cleanups, fn)
}

func (c *cliT) TempDir() string {
	dir, err := os.MkdirTemp("", "simtest-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "TempDir: %v\n", err)
		os.Exit(1)
	}
	c.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

// runCleanups fires every registered cleanup in LIFO order.
func (c *cliT) runCleanups() {
	for i := len(c.cleanups) - 1; i >= 0; i-- {
		c.cleanups[i]()
	}
	c.cleanups = nil
}

func main() {
	var (
		scenarioName string
		runAll       bool
		list         bool
	)
	flag.StringVar(&scenarioName, "scenario", "", "name of a single scenario to run")
	flag.BoolVar(&runAll, "all", false, "run every registered scenario")
	flag.BoolVar(&list, "list", false, "list every registered scenario and exit")
	flag.Parse()

	scenarios := sim.AllScenarios()

	if list || (scenarioName == "" && !runAll) {
		fmt.Println("Registered sim scenarios:")
		if len(scenarios) == 0 {
			fmt.Println("  (none — nothing has called sim.RegisterScenario yet)")
			return
		}
		for _, s := range scenarios {
			fmt.Printf("  • %-30s %s\n", s.Name, s.Description)
		}
		if !list {
			fmt.Println("\nRun one with:  -scenario <name>   or all with:  -all")
		}
		return
	}

	var failures int
	run := func(s sim.Scenario) {
		t := &cliT{}
		h := sim.NewHarness(t, sim.HarnessOptions{Out: os.Stdout})
		err := s.Run(h)
		t.runCleanups()
		if err != nil {
			fmt.Fprintf(os.Stderr, "FAIL: %s — %v\n\n", s.Name, err)
			failures++
			return
		}
		fmt.Printf("PASS: %s\n\n", s.Name)
	}

	if runAll {
		for _, s := range scenarios {
			run(s)
		}
	} else {
		s, ok := sim.LookupScenario(scenarioName)
		if !ok {
			fmt.Fprintf(os.Stderr, "unknown scenario: %q\n", scenarioName)
			os.Exit(2)
		}
		run(s)
	}

	if failures > 0 {
		os.Exit(1)
	}
}
