package layers

// Agent layer: Tool schemas.
// The hot tool definitions that the agent model sees on every turn.
// These are loaded from YAML manifests (tools/<name>/tool.yaml) at
// init time. The agent can load deferred tools via use_tools().
//
// This layer doesn't produce system prompt text — the tool schemas
// are passed as a separate `tools` parameter in the API call. But
// they still consume tokens in the agent's context window, so we
// calculate their size here for observability in `her shape`.

import (
	"encoding/json"
	"fmt"

	"her/tools"
)

func init() {
	Register(PromptLayer{
		Name:    "Tool Schemas (hot)",
		Order:   50, // before other agent layers — shows up first in shape
		Stream:  StreamAgent,
		Builder: buildAgentTools,
	})
}

func buildAgentTools(ctx *LayerContext) LayerResult {
	hotDefs := tools.HotToolDefs(ctx.Cfg)

	// Marshal the tool definitions to JSON to get a realistic token
	// estimate. The API sends these as JSON, so this reflects what
	// actually hits the model's context window.
	data, err := json.Marshal(hotDefs)
	if err != nil {
		return LayerResult{
			Detail: fmt.Sprintf("%d tools (marshal error)", len(hotDefs)),
		}
	}

	return LayerResult{
		// Content is empty because tool schemas aren't part of the system
		// prompt text — they're a separate API parameter. But we still
		// report the tokens so `her shape` shows the full picture.
		Content: "",
		Tokens:  estimateTokens(string(data)),
		Detail:  fmt.Sprintf("%d tools", len(hotDefs)),
	}
}
