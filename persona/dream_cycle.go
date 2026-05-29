package persona

import (
	"fmt"
	"strings"
	"time"

	"her/config"
	"her/embed"
	"her/llm"
	"her/memory"
	"her/tui"
)

// DreamCycleParams bundles everything a full dream cycle needs.
// Both the nightly timer (dreamer.go) and the /dream command
// (ExecDream) use this — one function, one set of steps.
type DreamCycleParams struct {
	LLM           *llm.Client   // persona/reflection model
	DreamLLM      *llm.Client   // memory dreamer model — nil disables consolidation
	ClassifierLLM *llm.Client   // persona quality gate — nil-safe
	Embed         *embed.Client // reflection dedup — nil-safe
	Store         memory.Store
	Cfg           *config.Config
	EventBus      *tui.Bus

	// ForceRewrite bypasses the min-days/min-reflections gate on persona
	// rewrite. true when the user explicitly runs /dream; false for the
	// nightly timer.
	ForceRewrite bool

	MinDays int // minimum days between rewrites; 0 defaults to 7
	MinRefl int // minimum unconsumed reflections; 0 defaults to 3
}

// DreamCycleResult summarizes what happened across all dream steps.
type DreamCycleResult struct {
	// Step 0: Consolidation
	ConsolidationRewrites int
	ConsolidationMerges   int
	ConsolidationExpires  int
	ConsolidationCreates  int
	ConsolidationError    error

	// Step 1: Reflection
	ReflectionError error

	// Step 2: Persona rewrite
	PersonaRewritten bool
	RewriteError     error

	// Step 3: Tomorrow's preload
	PreloadGenerated bool
	PreloadError     error

	// Accumulated cost across all dream steps.
	TotalCost float64
}

// Summary returns a human-readable summary of the dream cycle, suitable
// for the /dream command response or sim report.
func (r DreamCycleResult) Summary() string {
	var msg strings.Builder
	msg.WriteString("== Dream Complete ==\n\n")

	if r.ConsolidationError != nil {
		fmt.Fprintf(&msg, "Consolidation error: %v\n\n", r.ConsolidationError)
	} else if r.ConsolidationRewrites+r.ConsolidationMerges+r.ConsolidationExpires+r.ConsolidationCreates > 0 {
		fmt.Fprintf(&msg, "Consolidated: %d rewrites, %d merges, %d expires, %d creates\n\n",
			r.ConsolidationRewrites, r.ConsolidationMerges, r.ConsolidationExpires, r.ConsolidationCreates)
	} else {
		msg.WriteString("Memories look tidy — nothing to consolidate.\n\n")
	}

	if r.ReflectionError != nil {
		fmt.Fprintf(&msg, "Reflection error: %v\n\n", r.ReflectionError)
	}

	if r.PreloadGenerated {
		msg.WriteString("Tomorrow's preload generated.\n\n")
	} else if r.PreloadError != nil {
		fmt.Fprintf(&msg, "Preload error: %v\n\n", r.PreloadError)
	}

	if r.PersonaRewritten {
		msg.WriteString("Persona rewritten. Use /persona to see the update.\n\n")
	} else {
		msg.WriteString("No persona changes — not enough has shifted yet.\n\n")
	}

	if r.TotalCost > 0 {
		fmt.Fprintf(&msg, "Total cost: $%.4f", r.TotalCost)
	}

	return msg.String()
}

// RunDreamCycle executes the full 4-step dream cycle. This is the single
// entry point — both the nightly timer and the /dream command use it.
// Adding a new dream step means changing only this function.
func RunDreamCycle(p DreamCycleParams) DreamCycleResult {
	var result DreamCycleResult
	dreamStart := time.Now()

	if p.MinDays == 0 {
		p.MinDays = 7
	}
	if p.MinRefl == 0 {
		p.MinRefl = 3
	}

	log.Info("dreamer: running dream cycle")

	// Step 0: Memory consolidation.
	if p.DreamLLM != nil && p.Cfg.Dream.DreamEnabled() {
		dreamerResult := RunMemoryDreamer(MemoryDreamerParams{
			LLM:         p.DreamLLM,
			Store:       p.Store,
			EmbedClient: p.Embed,
			Cfg:         p.Cfg,
			EventBus:    p.EventBus,
		})
		result.ConsolidationRewrites = dreamerResult.Rewrites
		result.ConsolidationMerges = dreamerResult.Merges
		result.ConsolidationExpires = dreamerResult.Expires
		result.ConsolidationCreates = dreamerResult.Creates
		result.ConsolidationError = dreamerResult.Error
		if dreamerResult.Error != nil {
			log.Error("dreamer: memory consolidation failed", "err", dreamerResult.Error)
		} else {
			log.Infof("dreamer: %d rewrites, %d merges, %d expires, %d creates",
				dreamerResult.Rewrites, dreamerResult.Merges, dreamerResult.Expires, dreamerResult.Creates)
			emitPersonaEvent(p.EventBus, "dream_consolidate",
				fmt.Sprintf("%d rewrites, %d merges, %d expires, %d creates",
					dreamerResult.Rewrites, dreamerResult.Merges, dreamerResult.Expires, dreamerResult.Creates))
		}
	}

	// Step 1: Nightly reflection.
	if err := NightlyReflect(p.LLM, p.Store, p.Cfg, p.Cfg.Identity.Her, p.Cfg.Identity.User); err != nil {
		log.Error("dreamer: nightly reflection failed", "err", err)
		result.ReflectionError = err
	} else {
		emitPersonaEvent(p.EventBus, "dream_reflect", "nightly reflection complete")
	}

	// Step 2: Gated rewrite.
	rewritten, err := GatedRewrite(p.LLM, p.ClassifierLLM, p.Embed, p.Store,
		p.Cfg.Persona.PersonaFile, p.Cfg.Identity.Her, p.ForceRewrite, p.MinDays, p.MinRefl)
	if err != nil {
		log.Error("dreamer: gated rewrite failed", "err", err)
		result.RewriteError = err
	} else if rewritten {
		log.Info("dreamer: persona rewritten during dream cycle")
		emitPersonaEvent(p.EventBus, "dream_rewrite", "persona updated via dream")
	}
	result.PersonaRewritten = rewritten

	// Step 3: Tomorrow's preload.
	if p.Cfg.Dream.TomorrowPreload.Enabled {
		if err := RunTomorrowPreload(TomorrowPreloadParams{
			LLM:      p.LLM,
			Store:    p.Store,
			Cfg:      p.Cfg,
			EventBus: p.EventBus,
		}); err != nil {
			log.Error("dreamer: tomorrow preload failed", "err", err)
			result.PreloadError = err
		} else {
			result.PreloadGenerated = true
		}
	}

	// Sum all dream costs from the metrics table. Every LLM call in the
	// dream pipeline already saves to metrics with RoleDream — we query
	// the window rather than threading cost through every nested function.
	if cost, err := p.Store.CostSince(memory.RoleDream, dreamStart); err == nil {
		result.TotalCost = cost
	}

	log.Infof("dreamer: dream cycle complete | cost=$%.4f", result.TotalCost)
	return result
}
