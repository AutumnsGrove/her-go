package layers

// Layer 3: Current time.
// Always injected so the bot knows what time of day it is, what day
// of the week, etc. Without this, she has no sense of time at all.

import "time"

func init() {
	Register(PromptLayer{
		Name:    "Current Time",
		Order:   300,
		Stream:  StreamChat,
		Builder: buildChatTime,
	})
}

func buildChatTime(ctx *LayerContext) LayerResult {
	loc := time.Local
	now := time.Now().In(loc)
	content := "# Current Time\n" + now.Format("Monday, January 2, 2006 at 3:04 PM (MST)")
	return LayerResult{Content: content}
}
