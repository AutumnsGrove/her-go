package layers

// Agent layer: Semantically relevant user facts.
// The agent only sees facts relevant to the current message (via KNN).
// For deeper lookups, it uses its recall_memories tool on demand.
//
// Previously this injected ALL user facts, which would scale badly
// as the fact count grows. The recall_memories tool exists precisely
// for when the agent needs facts beyond what semantic search found.

import (
	"fmt"
	"strings"
)

func init() {
	Register(PromptLayer{
		Name:    "User Memories",
		Order:   400,
		Stream:  StreamAgent,
		Builder: buildAgentUserFacts,
	})
}

func buildAgentUserFacts(ctx *LayerContext) LayerResult {
	// Filter relevant facts to just user-subject ones.
	var count int
	var b strings.Builder
	b.WriteString("## Relevant User Memories\n\n")
	b.WriteString("These are the facts most relevant to the current message. To search for more, use `use_tools([\"memory\"])` then `recall_memories`.\n\n")

	for _, f := range ctx.RelevantFacts {
		if f.Subject != "user" {
			continue
		}
		fmt.Fprintf(&b, "- [ID=%d, %s] %s\n", f.ID, f.Category, f.Fact)
		count++
	}

	if count == 0 {
		return LayerResult{
			Content: "## Relevant User Memories\n\n(none matched this message — to search for specific facts, use `use_tools([\"memory\"])` then `recall_memories`)",
			Detail:  "0 facts",
		}
	}

	return LayerResult{
		Content: b.String(),
		Detail:  fmt.Sprintf("%d facts (semantic)", count),
	}
}
