// bot/introspection.go — Launches the introspection agent goroutine.
//
// The introspection agent is the 4th agent in the pipeline: it runs AFTER
// memory + mood complete (coordinated via sync.WaitGroup). It reflects on
// the turn's think traces and reply to extract self-observations.
package bot

import (
	"fmt"
	"os"
	"sync"

	"her/agent"
	"her/memory"
	"her/tools"
	"her/turn"
)

// launchIntrospectionAgent fires the introspection agent in a goroutine.
// It waits for the memory + mood agents to finish (via wg.Wait), snapshots
// self-memories and persona.md, then runs the introspection tool-calling loop.
//
// The phase is registered BEFORE the goroutine launches (same pattern as
// memory and mood) to prevent premature TurnEndEvent.
func (b *Bot) launchIntrospectionAgent(
	result *agent.RunResult,
	params agent.RunParams,
	wg *sync.WaitGroup,
	traceCallback tools.TraceCallback,
	tracker *turn.Tracker,
	lite *liteTraceState,
) {
	if b.introspectionLLM == nil {
		return
	}

	// Register the phase BEFORE launching the goroutine.
	var introPhase *turn.PhaseHandle
	if tracker != nil {
		introPhase = tracker.Begin("introspection")
	}

	go func() {
		defer func() {
			if r := recover(); r != nil {
				log.Error("introspection agent panic (recovered)", "panic", r)
			}
		}()
		if introPhase != nil {
			defer introPhase.Done(turn.PhaseMetrics{})
		}

		// Wait for memory + mood to finish so we see any self-memories
		// the memory agent just wrote.
		wg.Wait()

		// Snapshot self-memories AFTER the wait — captures anything the
		// memory agent saved during this turn.
		selfCards, err := params.Store.CardsBySubject("self")
		if err != nil {
			log.Error("introspection: failed to load self cards", "err", err)
			return
		}
		var selfMemories []memory.Memory
		for _, card := range selfCards {
			mems, err := params.Store.MemoriesByCard(card.ID)
			if err != nil {
				log.Warn("introspection: failed to load memories for card",
					"card", card.TopicSlug, "err", err)
				continue
			}
			selfMemories = append(selfMemories, mems...)
		}

		// Snapshot persona.md.
		var personaText string
		if b.cfg.Persona.PersonaFile != "" {
			if data, err := os.ReadFile(b.cfg.Persona.PersonaFile); err == nil {
				personaText = string(data)
			}
		}

		// result may be nil for fast-path turns (no driver ran).
		var thinkTraces []string
		var replyText string
		if result != nil {
			thinkTraces = result.ThinkTraces
			replyText = result.ReplyText
		}

		introResult := agent.RunIntrospectionAgent(
			agent.IntrospectionAgentInput{
				UserMessage:    params.ScrubbedUserMessage,
				ThinkTraces:    thinkTraces,
				ReplyText:      replyText,
				TriggerMsgID:   params.TriggerMsgID,
				ConversationID: params.ConversationID,
				SelfMemories:   selfMemories,
				PersonaText:    personaText,
			},
			agent.IntrospectionAgentParams{
				LLM:           b.introspectionLLM,
				ClassifierLLM: b.classifierLLM,
				Store:         b.store,
				EmbedClient:   b.embedClient,
				Cfg:           b.cfg,
				TraceCallback: traceCallback,
				EventBus:      b.eventBus,
				Phase:         introPhase,
			},
		)
		if lite != nil {
			lite.setIntrospection(fmt.Sprintf("🪡 %d self", introResult.SelfMemoriesSaved))
		}
	}()
}
