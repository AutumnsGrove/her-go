package layers

// Agent layer: Semantically relevant self facts.
// The agent sees self-facts that are relevant to the current message
// plus high-importance self-facts (backfilled for personality steering,
// same logic as the chat model). For everything else, recall_memories.

import (
	"fmt"
	"strings"
)

func init() {
	Register(PromptLayer{
		Name:    "Self Memories",
		Order:   500,
		Stream:  StreamAgent,
		Builder: buildAgentSelfFacts,
	})
}

func buildAgentSelfFacts(ctx *LayerContext) LayerResult {
	botName := ctx.Cfg.Identity.Her

	// Filter relevant facts to just self-subject ones.
	var count int
	var b strings.Builder
	fmt.Fprintf(&b, "## Relevant Self Memories (%s's own knowledge)\n\n", botName)
	b.WriteString("(Use recall_memories to search for more if needed)\n\n")

	for _, f := range ctx.RelevantFacts {
		if f.Subject != "self" {
			continue
		}
		fmt.Fprintf(&b, "- [ID=%d, %s, importance=%d] %s\n", f.ID, f.Category, f.Importance, f.Fact)
		count++
	}

	if count == 0 {
		return LayerResult{
			Content: fmt.Sprintf("## Relevant Self Memories (%s's own knowledge)\n\n(none matched — use recall_memories to search)", botName),
			Detail:  "0 facts",
		}
	}

	return LayerResult{
		Content: b.String(),
		Detail:  fmt.Sprintf("%d facts (semantic)", count),
	}
}
