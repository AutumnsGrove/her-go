package layers

// Layer 2.7: Tomorrow's Preload — auto-injected context from the dream cycle.
//
// The dream cycle's preload agent writes a short note about what to be
// ready to bring up. This layer injects it into the first chat turn of
// the day, then marks it consumed so it doesn't repeat. The note gives
// the chat model a "what's on my mind" section — the equivalent of
// Samantha arriving at a conversation already holding context.

func init() {
	Register(PromptLayer{
		Name:    "Tomorrow's Preload",
		Order:   270,
		Stream:  StreamChat,
		Builder: buildChatTomorrowPreload,
	})
}

func buildChatTomorrowPreload(ctx *LayerContext) LayerResult {
	if ctx.Store == nil {
		return LayerResult{}
	}

	preload, err := ctx.Store.ActiveTomorrowPreload()
	if err != nil || preload == nil {
		return LayerResult{}
	}

	content := "## What's on My Mind\n\n" + preload.Content + "\n"

	// Store the preload ID on the context so the reply handler can
	// consume it after delivery. We don't consume here because the
	// layer runs during prompt building — if the reply fails or the
	// turn is interrupted, we want the preload to survive for the
	// next attempt.
	ctx.PreloadID = preload.ID

	return LayerResult{
		Content: content,
		Detail:  "preload active",
	}
}
