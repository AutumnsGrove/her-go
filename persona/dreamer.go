// Package persona — dreamer.go runs the nightly persona evolution goroutine.
//
// The dreamer fires at a configurable hour each night (default 04:00 local time).
// It calls NightlyReflect (always) and then GatedRewrite (if gates pass). The gates
// prevent gratuitous rewrites: the persona only changes when enough time has passed
// AND enough reflections have accumulated.
//
// On startup, if >20 hours have elapsed since the last reflection, the dreamer runs
// a catch-up dream immediately — so a restart after a long gap doesn't skip a night.
package persona

import (
	"context"
	"time"

	"her/config"
	"her/embed"
	"her/llm"
	"her/memory"
	"her/trace"
	"her/tui"
)

// Register the persona trace stream. Order 150 puts it between main (100)
// and memory (200) — persona evolution context sits naturally between the
// driver's decision-making and the memory agent's fact extraction.
func init() {
	trace.Register(trace.Stream{Name: "persona", Order: 150, Label: "🪞 <b>persona</b>"})
}

// DreamerParams bundles everything the dreamer goroutine needs. Passing a
// single struct instead of many positional arguments makes call sites readable
// and easier to extend — same pattern used by agent.RunParams.
type DreamerParams struct {
	LLM           *llm.Client    // persona model for NightlyReflect and GatedRewrite
	DreamLLM      *llm.Client    // memory dreamer model — nil disables consolidation
	ClassifierLLM *llm.Client    // classifier for persona quality gate — nil-safe, gate skipped if nil
	Embed         *embed.Client  // embedding client for reflection dedup — nil-safe, dedup skipped if nil
	Store         memory.Store   // SQLite store for reading/writing persona state
	Cfg           *config.Config
	EventBus      *tui.Bus // may be nil (e.g., sim mode) — all emits are nil-safe

	DreamHour int // local hour to run (0-23); 0 defaults to 4 (04:00)
	MinDays   int // minimum days between rewrites; 0 defaults to 7
	MinRefl   int // minimum unconsumed reflections for rewrite; 0 defaults to 3
}

// StartDreamer launches the nightly dreaming goroutine. Call it with `go`
// in cmd/run.go after the bot is initialised. It runs until ctx is cancelled.
//
// The ctx.Done() check in the timer loop is how we get clean shutdown — when
// the bot receives SIGTERM, the context is cancelled and the goroutine exits
// at its next scheduled wake-up without leaving a dangling goroutine.
func StartDreamer(ctx context.Context, p DreamerParams) {
	// Apply defaults.
	if p.DreamHour == 0 {
		p.DreamHour = 4
	}
	if p.MinDays == 0 {
		p.MinDays = 7
	}
	if p.MinRefl == 0 {
		p.MinRefl = 3
	}

	log.Info("dreamer started", "dream_hour", p.DreamHour, "min_days", p.MinDays, "min_reflections", p.MinRefl)

	// Catch-up: if the last reflection was more than 20 hours ago (or never),
	// run a dream immediately rather than waiting until tonight's window.
	// This handles the case where the bot was offline overnight.
	state, err := p.Store.GetPersonaState()
	if err != nil {
		log.Warn("dreamer: failed to read persona state at startup", "err", err)
	}

	const catchUpThreshold = 20 * time.Hour
	if state.LastReflectionAt.IsZero() || time.Since(state.LastReflectionAt) > catchUpThreshold {
		log.Info("dreamer: catch-up dream running at startup")
		runDream(ctx, p)
	}

	// Loop: sleep until the next dream window, then run, then reschedule.
	// time.NewTimer is preferable to time.Sleep here because it's
	// cancellable — we can select{} across the timer and ctx.Done() together.
	for {
		next := durationUntilNextDream(time.Now(), p.DreamHour)
		log.Info("dreamer: next dream scheduled", "in", next.Round(time.Minute))

		timer := time.NewTimer(next)
		select {
		case <-ctx.Done():
			timer.Stop()
			log.Info("dreamer: context cancelled, shutting down")
			return
		case <-timer.C:
			runDream(ctx, p)
		}
	}
}

// runDream delegates to RunDreamCycle — the single entry point for all
// dream steps. The nightly timer uses ForceRewrite=false (respect cooldown).
func runDream(ctx context.Context, p DreamerParams) {
	select {
	case <-ctx.Done():
		return
	default:
	}

	RunDreamCycle(DreamCycleParams{
		LLM:           p.LLM,
		DreamLLM:      p.DreamLLM,
		ClassifierLLM: p.ClassifierLLM,
		Embed:         p.Embed,
		Store:         p.Store,
		Cfg:           p.Cfg,
		EventBus:      p.EventBus,
		ForceRewrite:  false,
		MinDays:       p.MinDays,
		MinRefl:       p.MinRefl,
	})
}

// durationUntilNextDream returns how long to sleep until the next occurrence
// of dreamHour:00:00 in local time. If that time has already passed today,
// it returns the duration until tomorrow at that hour.
//
// Example: if it's 14:30 and dreamHour=4, returns ~13.5 hours (until 04:00 tomorrow).
// If it's 02:00 and dreamHour=4, returns ~2 hours (until 04:00 today).
func durationUntilNextDream(now time.Time, dreamHour int) time.Duration {
	// Build today's target time in local timezone.
	y, m, d := now.Date()
	loc := now.Location()
	target := time.Date(y, m, d, dreamHour, 0, 0, 0, loc)

	// If the target is in the past (or within 1 minute — avoid near-zero sleeps),
	// push it to tomorrow.
	if target.Before(now.Add(time.Minute)) {
		target = target.Add(24 * time.Hour)
	}
	return target.Sub(now)
}

// emitPersonaEvent fires a tui.PersonaEvent on the event bus. The nil check
// lets this function be called safely from sim mode where there is no TUI.
func emitPersonaEvent(bus *tui.Bus, action, detail string) {
	if bus == nil {
		return
	}
	bus.Emit(tui.PersonaEvent{
		Time:   time.Now(),
		Action: action,
		Detail: detail,
	})
}
