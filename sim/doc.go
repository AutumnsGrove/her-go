// Package sim is the headless end-to-end simulator for her-go.
//
// It runs the full bot stack — scheduler, mood agent, memory store,
// Telegram wire layer, LLM calls — without ever talking to Telegram or
// OpenRouter. You call scenarios from a Go test (`go test ./sim/...`)
// or from the `her sim` CLI command. Nothing in the sim requires you to
// tap a button or read a chat.
//
// # What it's for
//
//   - Regression-test the mood pipeline end-to-end: inferred auto-log,
//     medium-confidence proposal, dedup hits, expiry sweeper, daily
//     rollup, manual /mood wizard. Each is a scenario.
//   - Reproduce bugs: turn a "happened once in Telegram" report into a
//     scenario that fails, then fix until it passes.
//   - Design-time sanity check: see what the user would have seen, in
//     order, without reaching for a phone.
//
// # How it hangs together
//
//   - FakeTransport (sim/telegram.go) implements the tiny subset of the
//     Telegram bot API the bot actually uses — Send, Edit, SendPhoto,
//     plus a Dispatch() entry point that fakes a user tapping an inline
//     button. Every interaction is recorded in order so assertions can
//     read the "chat transcript" after the scenario finishes.
//   - FakeClock (sim/clock.go) is an injectable time source. Scenarios
//     advance time with clock.Advance(d); the scheduler sees the jump
//     via the clock's Now() and fires anything that came due.
//   - FakeLLM (sim/llm.go) is an httptest.Server that responds to
//     OpenRouter-shaped requests with scripted replies keyed by the
//     calling model or prompt content. Useful for the mood agent: given
//     a transcript with "I'm exhausted" in it, reply with a high-
//     confidence "unpleasant/sad" mood proposal.
//   - Scenario (sim/scenario.go) ties them together. A scenario is a
//     list of Steps — each is a small function operating on the shared
//     Harness — and a list of Assertions evaluated at the end.
//
// # Running
//
//	# As a normal Go test
//	go test ./sim/...
//
//	# As a headless CLI (verbose output; useful for manual sanity checks)
//	go run ./sim/cmd/sim -scenario daily_rollup
//
// # Adding a scenario
//
// Put a new *_scenario.go (or *_test.go) file in sim/scenarios/ and
// register it from init() via sim.RegisterScenario(name, func(h *Harness)).
// The scenario is then runnable from both the test entry and the CLI.
//
// # What's NOT simulated
//
// The sim models the bot's external edges. It doesn't replace your
// actual sqlite-vec embeddings unless you opt in (embedDim=0 skips
// them), and it doesn't pretend to be a real LLM — you script replies
// deterministically. If a scenario passes in sim, you've got reasonable
// confidence the code is wired right; the remaining risk is that a real
// LLM replies differently than your script, which is exactly what the
// bot's fail-open classifier and style gates exist to catch.
package sim
