// Package layers implements a registry of prompt layers for both the agent
// and chat model streams. Each layer is a self-contained unit that produces
// a chunk of the system prompt — reading from disk, the DB, or runtime state.
//
// Layers register themselves via init() in their own files. The registry
// guarantees that adding a new layer = adding one file. No other files need
// to change. The `her shape` CLI command and the runtime prompt builder both
// consume the same registry, so they can never drift out of sync.
//
// This is the same pattern as the tool YAML registry (tools/loader.go) and
// handler registration (tools/registry.go), but for prompt assembly instead
// of tool dispatch.
package layers

import (
	"sort"
	"strings"

	"her/config"
	"her/embed"
	"her/memory"
)

// Stream identifies which model a layer belongs to.
// A layer tagged StreamChat only runs when building the reply-model prompt.
// A layer tagged StreamAgent only runs when building the driver agent context.
type Stream int

const (
	// StreamChat is the conversational model (configured via cfg.Chat).
	// Layers here produce the 9-layer system prompt for the reply tool.
	StreamChat Stream = iota

	// StreamAgent is the orchestrator model.
	// Layers here produce the user context message for the agent loop.
	StreamAgent
)

// String returns a human-readable name for the stream.
func (s Stream) String() string {
	switch s {
	case StreamChat:
		return "chat"
	case StreamAgent:
		return "agent"
	default:
		return "unknown"
	}
}

// LayerResult is what a layer builder returns. Content is the actual prompt
// text (empty string means the layer was skipped — no weather configured,
// no mood data, etc.). Tokens is the estimated token count. Detail is
// a human-readable note for the shape command ("5 facts", "10 messages").
type LayerResult struct {
	Name    string // layer name (copied from registration for convenience)
	Content string // the prompt text — empty means "layer skipped this turn"
	Tokens  int    // estimated token count (len/4 heuristic)
	Detail  string // human-readable detail: "5 facts", "10 msgs", etc.

	// InjectedMemories is set by the memory layer to pass observability data
	// back to the caller (for logging which memories made it into the prompt
	// and why). Most layers leave this nil.
	InjectedMemories []memory.InjectedMemory
}

// LayerContext carries everything a layer builder might need. This replaces
// the scattered parameters that currently flow through buildChatSystemPrompt
// and buildAgentContext. Not every layer uses every field — weather layers
// ignore EmbedClient, time layers ignore Store, etc.
type LayerContext struct {
	Store       *memory.Store
	Cfg         *config.Config
	EmbedClient *embed.Client

	// Runtime state — set by the agent before building the prompt.
	RelevantMemories    []memory.Memory       // KNN results for semantic injection
	ConversationSummary string                // from chat compaction
	AgentActionSummary  string                // from agent compaction (tool call history)
	RecentAgentActions  []memory.AgentAction  // recent tool calls kept in full fidelity
	ConversationID      string                // current conversation
	ScrubbedUserMessage string                // PII-scrubbed user input
	RecentMessages      []memory.Message      // sliding window for agent context
	HasImage            bool                  // user sent a photo
	OCRText             string                // pre-flight OCR extraction
	ExpenseContext      string                // from receipt scanning (if any)

	// Instruction and search context from the agent (for chat stream messages).
	Instruction   string
	SearchContext string

	// AgentPassedMemories holds memories the agent explicitly chose via recall_memories
	// and passed through the reply tool's facts parameter. When set, chat_memory.go
	// injects these instead of the auto-searched RelevantMemories.
	AgentPassedMemories []string
}

// PromptLayer defines a single layer in the prompt assembly pipeline.
// Each layer has a name, an order (for sorting), a stream tag, and a
// builder function that produces the layer's content.
//
// Order uses gaps (100, 200, 300...) so new layers can be inserted
// between existing ones without renumbering everything.
type PromptLayer struct {
	Name    string                        // human-readable: "Mood Trend", "Weather"
	Order   int                           // sort key — lower runs first
	Stream  Stream                        // which model this layer targets
	Builder func(*LayerContext) LayerResult // produces the layer's content
}

// registry holds all registered layers. Populated by init() calls
// in each layer file. Immutable after init phase completes.
var registry []PromptLayer

// Register adds a layer to the registry. Called from init() in each
// layer file — same pattern as tools.Register() and database/sql drivers.
//
// Because all layer files are in the same package, their init() functions
// run automatically. No blank imports needed (unlike the tool handlers
// in tools/<name>/handler.go, which live in separate packages).
func Register(layer PromptLayer) {
	registry = append(registry, layer)
}

// sorted returns layers for a given stream, sorted by Order.
func sorted(stream Stream) []PromptLayer {
	var result []PromptLayer
	for _, l := range registry {
		if l.Stream == stream {
			result = append(result, l)
		}
	}
	sort.Slice(result, func(i, j int) bool {
		return result[i].Order < result[j].Order
	})
	return result
}

// BuildAll runs every registered layer for the given stream and returns
// the concatenated prompt text plus individual results for observability.
// This is what the runtime calls — it replaces buildChatSystemPrompt and
// buildAgentContext.
//
// The separator between layers is "\n\n---\n\n" for the chat stream
// (matching the existing format) and "\n\n" for the agent stream.
//
// Layers with empty Content but non-zero Tokens are "overhead" layers —
// they report token usage for things assembled elsewhere (system prompt,
// tool schemas) but don't contribute text to the concatenated output.
// They're included in the results for observability but not in the prompt.
func BuildAll(stream Stream, ctx *LayerContext) (string, []LayerResult) {
	layers := sorted(stream)

	var parts []string
	var results []LayerResult

	for _, layer := range layers {
		result := layer.Builder(ctx)
		result.Name = layer.Name

		// Auto-calculate tokens if the builder didn't set them.
		if result.Content != "" && result.Tokens == 0 {
			result.Tokens = estimateTokens(result.Content)
		}

		// Skip layers with no content AND no token overhead.
		if result.Content == "" && result.Tokens == 0 {
			continue
		}

		// Only add content layers to the prompt text.
		if result.Content != "" {
			parts = append(parts, result.Content)
		}
		results = append(results, result)
	}

	sep := "\n\n---\n\n"
	if stream == StreamAgent {
		sep = "\n\n"
	}

	return strings.Join(parts, sep), results
}

// Shape runs every registered layer for the given stream but returns
// only the results (not the concatenated text). Used by the `her shape`
// CLI command to show what each layer contributes without actually
// building a prompt for an LLM call.
//
// Unlike BuildAll, Shape includes layers that returned empty content —
// they show up with 0 tokens so you can see they exist but were skipped.
func Shape(stream Stream, ctx *LayerContext) []LayerResult {
	layers := sorted(stream)
	var results []LayerResult

	for _, layer := range layers {
		result := layer.Builder(ctx)
		result.Name = layer.Name
		if result.Content != "" && result.Tokens == 0 {
			result.Tokens = estimateTokens(result.Content)
		}
		results = append(results, result)
	}
	return results
}

// estimateTokens gives a rough token count for English text.
// Same heuristic as compact.estimateTokens — ~4 characters per token.
func estimateTokens(s string) int {
	return len(s) / 4
}
