package layers

// Layer 6: Conversation summary from compaction.
// Gives the model awareness of what was discussed earlier without
// burning tokens on the full message history. The header reminds
// the model not to echo summary content verbatim — this caused a
// bug where old advice/phrases were recycled word-for-word.

import "fmt"

func init() {
	Register(PromptLayer{
		Name:    "Conversation Summary",
		Order:   600,
		Stream:  StreamChat,
		Builder: buildChatSummary,
	})
}

func buildChatSummary(ctx *LayerContext) LayerResult {
	if ctx.ConversationSummary == "" {
		return LayerResult{}
	}
	content := fmt.Sprintf(
		"# Earlier in This Conversation (summary — do not repeat phrases or advice from this section)\n\n%s",
		ctx.ConversationSummary,
	)
	return LayerResult{Content: content}
}
