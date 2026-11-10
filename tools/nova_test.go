package tools

// Tests for the Nova tool bridge (tools/nova.go).
//
// The most important one is the schema equality check: a tool definition is part
// of the prompt, so if the bridged version serialises differently the provider
// re-keys its prompt cache and every turn costs full price. That is a silent
// regression, which is exactly what a byte-equality test catches.

import (
	"context"
	"encoding/json"
	"sort"
	"testing"

	"github.com/AutumnsGrove/nova/tool"

	"her/llm"
)

// TestNovaTools_SchemaMatchesLegacyDefinition pins the wire shape of every
// registered tool: the Nova Info() definition must serialise to exactly the same
// JSON as the llm.ToolDef the legacy loop sends.
func TestNovaTools_SchemaMatchesLegacyDefinition(t *testing.T) {
	names := make([]string, 0, len(toolDefs))
	for name := range toolDefs {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		t.Fatal("no tools loaded — the embedded YAML loader produced nothing")
	}

	for _, name := range names {
		t.Run(name, func(t *testing.T) {
			def, ok := LookupDef(name)
			if !ok {
				t.Fatalf("LookupDef(%q) missing", name)
			}

			bridged := NovaTools([]llm.ToolDef{def})
			if len(bridged) != 1 {
				t.Fatalf("NovaTools returned %d tools, want 1", len(bridged))
			}

			legacyJSON, err := json.Marshal(def)
			if err != nil {
				t.Fatalf("marshalling legacy def: %v", err)
			}
			novaJSON, err := json.Marshal(bridged[0].Info().Def())
			if err != nil {
				t.Fatalf("marshalling nova def: %v", err)
			}
			if string(legacyJSON) != string(novaJSON) {
				t.Errorf("schema drift for %q:\n legacy: %s\n nova:   %s", name, legacyJSON, novaJSON)
			}
		})
	}
}

// TestNovaTools_RunDelegatesToRegistry checks the bridge executes through
// tools.Execute, so the malformed-JSON guard and error-prefix convention are the
// ones her-go already documents.
func TestNovaTools_RunDelegatesToRegistry(t *testing.T) {
	def, ok := LookupDef("think")
	if !ok {
		t.Skip("think tool not registered")
	}
	bridged := NovaTools([]llm.ToolDef{def})[0]

	nc := &tool.Context{Data: &Context{}}

	// Malformed JSON is rejected by the registry's guard, as it always was.
	res, err := bridged.Run(context.Background(), json.RawMessage(`{"thought": `), nc)
	if err != nil {
		t.Fatalf("Run returned an error, want a result string: %v", err)
	}
	if !res.IsError {
		t.Error("malformed arguments should be an error result")
	}
	if want := "error: malformed JSON in arguments"; !hasPrefix(res.Content, want) {
		t.Errorf("result = %q, want prefix %q", res.Content, want)
	}
}

// TestNovaTools_MissingToolDataIsAnErrorResult covers the wiring bug: without a
// *Context on ToolData the tool cannot run, and the model should get a result it
// can move past instead of a panic.
func TestNovaTools_MissingToolDataIsAnErrorResult(t *testing.T) {
	def, ok := LookupDef("think")
	if !ok {
		t.Skip("think tool not registered")
	}
	bridged := NovaTools([]llm.ToolDef{def})[0]

	res, err := bridged.Run(context.Background(), json.RawMessage(`{"thought":"x"}`), &tool.Context{})
	if err != nil {
		t.Fatalf("Run returned an error: %v", err)
	}
	if !res.IsError {
		t.Error("a missing dependency bundle should be an error result")
	}
}

// TestNovaTools_NotParallel documents the sequencing rule: no her-go tool may
// run concurrently with another in the same batch.
func TestNovaTools_NotParallel(t *testing.T) {
	def, ok := LookupDef("think")
	if !ok {
		t.Skip("think tool not registered")
	}
	if NovaTools([]llm.ToolDef{def})[0].Info().Parallel {
		t.Error("bridged tools must not be marked parallel")
	}
}

// TestIsErrorResult pins the prefix convention both loops depend on.
func TestIsErrorResult(t *testing.T) {
	cases := map[string]bool{
		"error: malformed JSON":  true,
		"rejected: too similar":  true,
		"saved user memory ID=1": false,
		"Error: capitalised":     false, // the convention is lowercase on purpose
		"":                       false,
	}
	for result, want := range cases {
		if got := IsErrorResult(result); got != want {
			t.Errorf("IsErrorResult(%q) = %v, want %v", result, got, want)
		}
	}
}

func hasPrefix(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
