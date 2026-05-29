// bot/mood.go — General mood agent wiring (transport-agnostic).
//
// initMood creates the mood.Runner with config defaults and wires
// Telegram-specific parts only when b.tb is non-nil. launchMoodAgent
// fires the runner in a goroutine after each reply.
//
// Telegram-specific mood UI (proposals, sweeper, graph, callbacks)
// lives in tg_mood.go.
package bot

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"her/mood"
	"her/tools"
	"her/tui"
	"her/turn"
)

// initMood wires the mood runner onto the Bot struct. Called from both
// New() (Telegram) and NewDev() (gateway) when the mood agent LLM is
// configured. Telegram-specific parts (sweeper, inline button handlers)
// are gated behind b.tb != nil.
func (b *Bot) initMood() error {
	vocab := mood.Default()
	if b.cfg.Mood.VocabPath != "" {
		v, err := mood.LoadVocab(b.cfg.Mood.VocabPath)
		if err != nil {
			return fmt.Errorf("loading mood vocab: %w", err)
		}
		vocab = v
	}
	b.moodVocab = vocab

	// Fill in AgentConfig defaults from config.yaml.
	high := b.cfg.Mood.ConfidenceHigh
	if high == 0 {
		high = 0.75
	}
	low := b.cfg.Mood.ConfidenceLow
	if low == 0 {
		low = 0.40
	}
	dedupWin := time.Duration(b.cfg.Mood.DedupWindowMinutes) * time.Minute
	if dedupWin == 0 {
		dedupWin = 2 * time.Hour
	}
	dedupSim := b.cfg.Mood.DedupSimilarity
	if dedupSim == 0 {
		dedupSim = 0.80
	}
	proposalExpiry := time.Duration(b.cfg.Mood.ProposalExpiryMinutes) * time.Minute
	if proposalExpiry == 0 {
		proposalExpiry = 30 * time.Minute
	}
	ctxTurns := b.cfg.Mood.ContextTurns
	if ctxTurns == 0 {
		ctxTurns = 5
	}

	// Embed bridge: mood.Deps expects a context-aware signature;
	// embed.Client.Embed only takes text.
	embedFn := func(_ context.Context, text string) ([]float32, error) {
		if b.embedClient == nil {
			return nil, nil
		}
		return b.embedClient.Embed(text)
	}

	// Derive the prompt directory from the main prompt file path —
	// mood_agent_prompt.md lives alongside prompt.md in the project root.
	promptDir := filepath.Dir(b.cfg.Persona.PromptFile)

	updateWin := time.Duration(b.cfg.Mood.UpdateWindowMinutes) * time.Minute

	b.moodRunner = &mood.Runner{
		Deps: mood.Deps{
			LLM:        b.moodAgentLLM,
			Classifier: b.classifierLLM,
			Store:      b.store,
			Vocab:      vocab,
			Embed:      embedFn,
			Propose:    b.sendMoodProposal,
			PromptDir:  promptDir,
		},
		Config: mood.AgentConfig{
			ContextTurns:    ctxTurns,
			ConfidenceHigh:  high,
			ConfidenceLow:   low,
			DedupWindow:     dedupWin,
			DedupSimilarity: dedupSim,
			UpdateWindow:    updateWin,
			ProposalExpiry:  proposalExpiry,
			SessionGap:      time.Duration(b.cfg.Mood.SessionGapMinutes) * time.Minute,
		},
	}

	// Telegram-specific: sweeper + inline button handlers.
	b.initMoodTelegram()

	return nil
}

// launchMoodAgent fires mood.Runner.RunForConversation in a goroutine.
// Called from runAgent after the main reply is sent. No-op when the
// mood runner isn't configured.
//
// The tracker manages the phase lifecycle: Begin is called here
// (before launching the goroutine to prevent a race) and Done fires
// inside the goroutine when the mood agent finishes.
func (b *Bot) launchMoodAgent(convID string, trace tools.TraceCallback, tracker *turn.Tracker, introspectionWG *sync.WaitGroup) {
	if b.moodRunner == nil || convID == "" {
		return
	}

	// Begin the mood phase BEFORE launching the goroutine. This
	// increments the Tracker's pending count so TurnEndEvent doesn't
	// fire prematurely if the main + memory phases finish first.
	var moodPhase *turn.PhaseHandle
	if tracker != nil {
		moodPhase = tracker.Begin("mood")
	}

	// Signal introspection WaitGroup — Add before goroutine, Done inside.
	if introspectionWG != nil {
		introspectionWG.Add(1)
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("mood agent panic (recovered)", "panic", r)
			}
		}()
		if introspectionWG != nil {
			defer introspectionWG.Done()
		}
		if moodPhase != nil {
			defer moodPhase.Done(turn.PhaseMetrics{})
		}

		// 60s timeout — mood agent does one LLM call plus an
		// optional classifier pass.
		var res mood.Result
		if trace != nil {
			res = b.moodRunner.RunForConversationWithTrace(
				context.Background(), convID, 60*time.Second, trace,
			)
		} else {
			res = b.moodRunner.RunForConversationWithTimeout(
				context.Background(), convID, 60*time.Second,
			)
		}

		if res.Action == mood.ActionErrored {
			log.Warn("mood agent errored", "reason", res.Reason)
		}

		// Emit a MoodEvent so the TUI/gateway shows what happened.
		if moodPhase != nil {
			var labels []string
			var valence int
			var confidence float64
			if res.Inference != nil {
				labels = res.Inference.Labels
				valence = res.Inference.Valence
				confidence = res.Confidence
			}
			moodPhase.Emit(tui.MoodEvent{
				Time:       time.Now(),
				TurnID:     moodPhase.TurnID(),
				Action:     string(res.Action),
				Valence:    valence,
				Labels:     labels,
				Confidence: confidence,
				Reason:     res.Reason,
			})
		}
	}()
}
