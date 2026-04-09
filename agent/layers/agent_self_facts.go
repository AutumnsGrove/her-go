package layers

// Agent layer: Semantically relevant self facts.
// The agent sees self-facts that are relevant to the current message
// (via KNN). For everything else, recall_memories.

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
	b.WriteString("These are the self-observations most relevant to the current message. To search for more, use `use_tools([\"memory\"])` then `recall_memories`.\n\n")

	for _, f := range ctx.RelevantFacts {
		if f.Subject != "self" {
			continue
		}
		fmt.Fprintf(&b, "- [ID=%d, %s] %s\n", f.ID, f.Category, f.Fact)
		count++
	}

	if count == 0 {
		return LayerResult{
			Content: fmt.Sprintf("## Relevant Self Memories (%s's own knowledge)\n\n(none matched this message — to search for specific self-knowledge, use `use_tools([\"memory\"])` then `recall_memories`)", botName),
			Detail:  "0 facts",
		}
	}

	return LayerResult{
		Content: b.String(),
		Detail:  fmt.Sprintf("%d facts (semantic)", count),
	}
}
