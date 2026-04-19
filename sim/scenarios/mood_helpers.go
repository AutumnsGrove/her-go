package scenarios

import (
	"context"
	"time"

	"her/llm"
	"her/mood"
	"her/memory"
	"her/sim"
)

// moodDeps builds a mood.Deps bundle wired to the Harness fakes.
// LLM points at the FakeLLM httptest server, Embed produces a tiny
// deterministic vector, and Propose routes through the FakeTransport
// so proposal messages show up in h.Transport.Events().
//
// The dimension must match whatever EmbedDim the Harness was built
// with — pass 0 (no embedding) when the scenario doesn't need KNN.
func moodDeps(h *sim.Harness, embedDim int) mood.Deps {
	return mood.Deps{
		LLM:   llm.NewClient(h.LLM.URL(), "sim-key", "mood-model", 0.1, 256),
		Store: h.Store,
		Vocab: mood.Default(),
		Clock: h.Clock.Now,
		Embed: func(_ context.Context, _ string) ([]float32, error) {
			if embedDim == 0 {
				return nil, nil
			}
			// Deterministic constant vector. Good enough for dedup
			// scenarios where identical inputs should trip the KNN
			// threshold; tests that want DIFFERENT vectors from
			// DIFFERENT inputs should override this.
			v := make([]float32, embedDim)
			for i := range v {
				v[i] = 0.5
			}
			return v, nil
		},
		Propose: func(_ context.Context, entry *memory.MoodEntry, _ time.Time) (int64, int64, error) {
			// Build an inline keyboard that mirrors what the real
			// Telegram proposal will look like: three buttons,
			// Log / Edit / Drop. The real bot handler will use the
			// same Unique so the callback router matches.
			summary := "I'm reading this as "
			if entry.Valence <= 3 {
				summary += "unpleasant"
			} else if entry.Valence == 4 {
				summary += "neutral"
			} else {
				summary += "pleasant"
			}
			summary += ". Log it?"

			buttons := []sim.Button{
				{Unique: "mood_proposal", Label: "✅ Log it", Data: "confirm"},
				{Unique: "mood_proposal", Label: "✏️ Edit", Data: "edit"},
				{Unique: "mood_proposal", Label: "❌ No", Data: "reject"},
			}
			msgID, err := h.Transport.Send(h.ChatID, summary, buttons...)
			if err != nil {
				return 0, 0, err
			}
			return h.ChatID, int64(msgID), nil
		},
	}
}

// runMood is a short helper that calls mood.RunAgent with the Harness
// and returns the Result. Every mood scenario uses this.
func runMood(h *sim.Harness, cfg mood.AgentConfig, embedDim int, turns []mood.Turn) mood.Result {
	return mood.RunAgent(h.Ctx, moodDeps(h, embedDim), cfg, turns)
}
