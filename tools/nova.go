// Bridge between her-go's YAML-driven tool registry and Nova's tool contract.
//
// Step 6 of the Nova migration (docs/plans/PLAN-nova-migration.md). her-go owns
// tool definitions in `tools/<name>/tool.yaml`, parsed by loader.go into
// []llm.ToolDef, and dispatched by name through tools.Execute. Nova's agent loop
// wants []tool.Tool: an object with an Info() definition and a Run() method.
//
// This file is the adapter. It does no work of its own:
//
//   - Info() reuses the same manifest-derived definition the legacy loop sends,
//     converted to Nova's provider.ToolDef shape.
//   - Run() calls tools.Execute with the same arguments and context, so the
//     malformed-JSON guard, the unknown-tool message and every handler behave
//     byte-identically.
//
// Deliberately NOT reimplemented here: JSON validation, error-string detection,
// trace formatting, deferred loading. They already live in the tools package
// (the owning package); duplicating them in the bridge would be exactly the
// capability leak the project's ownership map forbids.
package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/AutumnsGrove/nova/tool"

	"her/llm"
)

// NovaTools converts an agent's tool definitions into Nova tools.
//
// The returned tools share her-go's dispatch path, so they must be paired with a
// tool.Context whose Data field holds the *tools.Context for this run:
//
//	ag := agent.Config{ToolData: tctx, Tools: tools.NovaTools(defs)}
//
// That's the one wiring rule — Nova never learns what a *tools.Context is, and
// her-go never learns how Nova dispatches. The type assertion happens inside the
// wrapper, where the two worlds meet.
func NovaTools(defs []llm.ToolDef) []tool.Tool {
	out := make([]tool.Tool, 0, len(defs))
	for _, def := range defs {
		out = append(out, novaTool{def: def})
	}
	return out
}

// novaTool is one bridged tool. It is a struct rather than a closure so the
// name is visible in stack traces and `%v` output.
type novaTool struct {
	def llm.ToolDef
}

// Info implements tool.Tool. The parameters come from the same manifest field
// the legacy loop sends, so the model sees an identical schema — including the
// property ordering, which matters because a changed schema invalidates the
// provider's prompt cache (delta D7).
func (t novaTool) Info() tool.Info {
	params, _ := t.def.Function.Parameters.(map[string]any)
	if params == nil {
		params = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return tool.Info{
		Name:        t.def.Function.Name,
		Description: t.def.Function.Description,
		Parameters:  params,
		// Keep tool execution strictly sequential: her-go's agents assume each
		// call sees the previous result (think → reply → done). Nova's loop runs
		// a batch in parallel when every tool in it allows it, so nothing is
		// marked parallel here.
		Parallel: false,
	}
}

// Run implements tool.Tool by delegating to the registry.
//
// It never returns a Go error: her-go expresses tool failure as a result string
// starting with "error:" or "rejected:" (the loop and the models are built
// around that convention), and Nova's contract maps a returned error to a
// generic failed result with different wording. Returning the string keeps the
// exact text the models have been trained on and prompted with.
func (t novaTool) Run(ctx context.Context, args json.RawMessage, tctx *tool.Context) (tool.Result, error) {
	// The run's dependency bundle travels on ToolData. A missing or wrong type
	// is a wiring bug, not a model error: say so loudly, and give the model a
	// result it can move past rather than a panic mid-turn.
	appCtx, ok := tctx.Data.(*Context)
	if !ok {
		return tool.Result{
			Content: fmt.Sprintf("error: tool %q has no her-go context (ToolData is %T)", t.def.Function.Name, tctx.Data),
			IsError: true,
		}, nil
	}

	result := Execute(t.def.Function.Name, string(args), appCtx)
	return tool.Result{
		Content: result,
		IsError: IsErrorResult(result),
	}, nil
}

// IsErrorResult is the project's single definition of "this tool call failed".
// Both the legacy loop and the Nova bridge call it, so the two cannot drift
// apart as prompts and traces depend on the exact prefixes.
func IsErrorResult(result string) bool {
	return strings.HasPrefix(result, "error:") || strings.HasPrefix(result, "rejected:")
}
