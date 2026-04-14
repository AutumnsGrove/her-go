package tools

import (
	"encoding/json"
	"fmt"

	"her/logger"
)

// log is the package-level logger for the tools package.
var log = logger.WithPrefix("tools")

// Handler is the uniform function signature every tool handler must match.
// argsJSON is the raw JSON arguments from the model's tool call.
// ctx provides access to all dependencies (LLM clients, store, callbacks, etc.).
// The return value is a result string sent back to the agent model.
//
// This is like a Python protocol / interface — every handler conforms to
// the same shape so we can store them all in a map and dispatch dynamically.
type Handler func(argsJSON string, ctx *Context) string

// toolHandlers maps tool names to their handler functions. Each tool
// registers itself here via Register() in its init() function.
// This replaces the big switch statement that used to live in agent.go.
var toolHandlers = map[string]Handler{}

// Register adds a tool handler to the registry. Called from each tool's
// init() function (e.g., tools/reply/handler.go calls Register("reply", Handle)).
//
// Panics if a handler is already registered for the given name — this
// catches duplicate registrations at startup rather than silently
// overwriting, which would be a nasty bug to track down.
func Register(name string, h Handler) {
	if _, exists := toolHandlers[name]; exists {
		// Duplicate registrations can happen if a tool's init() fires twice
		// (e.g., imported via two paths). Log and skip rather than crashing —
		// the first registration wins, which is the correct behavior anyway.
		log.Warn("tools: duplicate handler registration, skipping", "tool", name)
		return
	}
	toolHandlers[name] = h
	log.Debug("registered tool handler", "tool", name)
}

// Execute runs a tool by name. It validates the JSON arguments before
// dispatching to the registered handler. If the tool name is unknown
// (no handler registered), it returns an error string.
//
// This replaces the executeTool switch statement in agent.go. The agent
// loop calls this instead of switching on tc.Function.Name.
func Execute(name, argsJSON string, ctx *Context) string {
	// Validate JSON before dispatching. Truncated tool calls happen when
	// the model hits max_tokens while generating the arguments JSON.
	// Rather than letting each handler fail with a confusing parse error,
	// give the model clear feedback so it can self-correct.
	if argsJSON != "" && !json.Valid([]byte(argsJSON)) {
		return fmt.Sprintf(
			"error: malformed JSON in arguments (likely truncated by token limit). "+
				"Please retry with shorter arguments. Got: %s",
			truncateStr(argsJSON, 100),
		)
	}

	handler, ok := toolHandlers[name]
	if !ok {
		return fmt.Sprintf("unknown tool: %s", name)
	}

	return handler(argsJSON, ctx)
}

// HasHandler returns true if a handler is registered for the given tool name.
func HasHandler(name string) bool {
	_, ok := toolHandlers[name]
	return ok
}

// truncateStr cuts a string to maxLen characters, appending "..." if truncated.
func truncateStr(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
