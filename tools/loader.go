package tools

// This file handles loading tool definitions from YAML manifests.
// Each tool has a tool.yaml in its directory (tools/<name>/tool.yaml)
// that declares its schema, hot/deferred status, and category.
//
// At init time, the embedded YAML files are parsed into llm.ToolDef
// structs that the agent can pass to the LLM API. This replaces the
// 680-line allToolDefs() function that used to define everything as
// Go struct literals.

import (
	"embed"
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"her/config"
	"her/llm"
)

// ---------------------------------------------------------------------------
// YAML schema types — these map directly to the tool.yaml format
// ---------------------------------------------------------------------------

// toolManifest is the top-level structure of a tool.yaml file.
type toolManifest struct {
	Name        string         `yaml:"name"`
	Description string         `yaml:"description"`
	Hint        string         `yaml:"hint,omitempty"`
	Hot         bool           `yaml:"hot"`
	Category    string         `yaml:"category,omitempty"`
	Parameters  parametersDef  `yaml:"parameters"`
	Trace       *traceSpec     `yaml:"trace,omitempty"`
}

// parametersDef represents the JSON Schema "parameters" block.
// We parse it as a generic structure because tool parameters can
// have nested objects (like scan_receipt's "items" array).
//
// This is intentionally flexible — the YAML mirrors JSON Schema
// syntax so the loader can convert it 1:1 to the map[string]interface{}
// that the OpenAI tool format expects.
type parametersDef struct {
	Type       string                    `yaml:"type"`
	Properties map[string]propertyDef    `yaml:"properties"`
	Required   []string                  `yaml:"required,omitempty"`
}

// propertyDef represents a single parameter in the JSON Schema.
type propertyDef struct {
	Type        string        `yaml:"type"`
	Description string        `yaml:"description,omitempty"`
	Enum        []string      `yaml:"enum,omitempty"`
	Minimum     *float64      `yaml:"minimum,omitempty"`
	Maximum     *float64      `yaml:"maximum,omitempty"`
	// For nested objects (e.g., array items)
	Items       *propertyDef  `yaml:"items,omitempty"`
	Properties  map[string]propertyDef `yaml:"properties,omitempty"`
	Required    []string      `yaml:"required,omitempty"`
}

// ---------------------------------------------------------------------------
// Compiled state — built once at init(), immutable after that
// ---------------------------------------------------------------------------

// toolDefs maps tool name → llm.ToolDef, built from YAML at init.
// This is the source of truth for all tool schemas.
var toolDefs = map[string]llm.ToolDef{}

// hotTools lists tool names where hot: true in the YAML.
var hotTools []string

// categories maps category name → list of tool names.
// Built from the "category" field in each tool's YAML.
var categories = map[string][]string{}

// hotToolHints maps hot tool name → terse one-liner for the agent prompt.
// Built from the "hint" field in each hot tool's YAML.
var hotToolHints = map[string]string{}

// ---------------------------------------------------------------------------
// Embedded YAML files — baked into the binary at compile time
// ---------------------------------------------------------------------------

// toolYAMLs embeds every tool.yaml from subdirectories. The glob
// pattern */tool.yaml matches one level deep: think/tool.yaml,
// done/tool.yaml, etc. This means adding a new tool just requires
// creating the directory and files — no registration in a central list.
//
//go:embed */tool.yaml
var toolYAMLs embed.FS

// categoriesYAML embeds the category hints file. This is separate from
// toolYAMLs because it lives in the tools/ root, not a subdirectory.
//
//go:embed categories.yaml
var categoriesYAML []byte

// categoryHintDef is the YAML structure for a single category in categories.yaml.
type categoryHintDef struct {
	Hint string `yaml:"hint"`
}

// categoryHints maps category name → agent-facing "when to use" hint.
// Built from categories.yaml at init time.
var categoryHints = map[string]string{}

// ---------------------------------------------------------------------------
// Loader — runs at init, parses YAML into llm.ToolDef structs
// ---------------------------------------------------------------------------

func init() {
	entries, err := fs.ReadDir(toolYAMLs, ".")
	if err != nil {
		panic(fmt.Sprintf("tools: failed to read embedded tool YAMLs: %v", err))
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		yamlPath := filepath.Join(entry.Name(), "tool.yaml")
		data, err := toolYAMLs.ReadFile(yamlPath)
		if err != nil {
			// Directory exists but no tool.yaml — skip silently.
			// This can happen if a tool only has handler.go (not yet
			// migrated to YAML).
			continue
		}

		var manifest toolManifest
		if err := yaml.Unmarshal(data, &manifest); err != nil {
			// Bad YAML in one tool shouldn't kill the whole bot — log and skip.
			// The remaining tools still load. Infrastructure-level failures
			// (embedded FS unreadable, categories.yaml broken) still panic below.
			log.Warn("tools: failed to parse YAML, skipping tool", "path", yamlPath, "err", err)
			continue
		}

		// Validate that the directory name matches the tool name.
		// This catches copy-paste errors where someone copies a tool
		// directory but forgets to update the name field.
		if manifest.Name != entry.Name() {
			log.Warn("tools: directory/name mismatch, skipping tool",
				"dir", entry.Name(), "name", manifest.Name)
			continue
		}

		// Convert the YAML manifest to an llm.ToolDef.
		def := llm.ToolDef{
			Type: "function",
			Function: llm.ToolFunctionDef{
				Name:        manifest.Name,
				Description: manifest.Description,
				Parameters:  convertParameters(manifest.Parameters),
			},
		}

		toolDefs[manifest.Name] = def

		if manifest.Hot {
			hotTools = append(hotTools, manifest.Name)
			if manifest.Hint != "" {
				hotToolHints[manifest.Name] = manifest.Hint
			}
		}

		if manifest.Category != "" {
			categories[manifest.Category] = append(
				categories[manifest.Category], manifest.Name,
			)
		}

		// Compile trace spec if present.
		if manifest.Trace != nil {
			traceRegistry[manifest.Name] = compileTraceSpec(manifest.Name, *manifest.Trace)
		}

		log.Debug("loaded tool from YAML", "tool", manifest.Name,
			"hot", manifest.Hot, "category", manifest.Category)
	}

	// Sort for deterministic output (map iteration order is random in Go).
	sort.Strings(hotTools)
	for cat := range categories {
		sort.Strings(categories[cat])
	}

	// Parse category hints from categories.yaml.
	var rawHints map[string]categoryHintDef
	if err := yaml.Unmarshal(categoriesYAML, &rawHints); err != nil {
		panic(fmt.Sprintf("tools: failed to parse categories.yaml: %v", err))
	}
	for name, def := range rawHints {
		categoryHints[name] = def.Hint
	}

	// Register trace spec for use_tools — it has no YAML file since its
	// definition is dynamically generated, but it still needs a trace format.
	traceRegistry["use_tools"] = compileTraceSpec("use_tools", traceSpec{
		Emoji:  "🔧",
		Format: "{{.Result | truncate 100 | escape}}",
	})

	log.Infof("loaded %d tool definitions from YAML (%d hot, %d categories)",
		len(toolDefs), len(hotTools), len(categories))
}

// convertParameters turns the YAML parametersDef into the
// map[string]interface{} that the OpenAI tool format expects.
// This is a recursive conversion — nested objects and arrays
// are handled by convertProperty.
func convertParameters(p parametersDef) map[string]interface{} {
	result := map[string]interface{}{
		"type": p.Type,
	}

	props := map[string]interface{}{}
	for name, prop := range p.Properties {
		props[name] = convertProperty(prop)
	}
	result["properties"] = props

	if len(p.Required) > 0 {
		result["required"] = p.Required
	}

	return result
}

// convertProperty converts a single YAML property definition to the
// map[string]interface{} format. Handles enums, min/max constraints,
// nested objects, and array items recursively.
func convertProperty(p propertyDef) map[string]interface{} {
	result := map[string]interface{}{
		"type": p.Type,
	}

	if p.Description != "" {
		result["description"] = p.Description
	}
	if len(p.Enum) > 0 {
		result["enum"] = p.Enum
	}
	if p.Minimum != nil {
		result["minimum"] = *p.Minimum
	}
	if p.Maximum != nil {
		result["maximum"] = *p.Maximum
	}

	// Nested object (e.g., items in an array of objects)
	if p.Items != nil {
		result["items"] = convertProperty(*p.Items)
	}

	// Object with sub-properties
	if len(p.Properties) > 0 {
		subProps := map[string]interface{}{}
		for name, sub := range p.Properties {
			subProps[name] = convertProperty(sub)
		}
		result["properties"] = subProps
	}

	if len(p.Required) > 0 {
		result["required"] = p.Required
	}

	return result
}

// ---------------------------------------------------------------------------
// Public API — used by agent to build tool lists
// ---------------------------------------------------------------------------

// LookupDef returns the llm.ToolDef for a tool loaded from YAML.
// Returns the def and true if found, zero value and false otherwise.
func LookupDef(name string) (llm.ToolDef, bool) {
	def, ok := toolDefs[name]
	return def, ok
}

// HotToolNames returns the names of all tools marked hot: true in YAML.
func HotToolNames() []string {
	return hotTools
}

// Categories returns the category → tool names map built from YAML.
func Categories() map[string][]string {
	return categories
}

// CategoryDescription builds a human-readable string listing all
// categories and their member tools. Used by the use_tools meta-tool
// to tell the agent what categories are available.
func CategoryDescription() string {
	// Sort category names for deterministic output.
	catNames := make([]string, 0, len(categories))
	for name := range categories {
		catNames = append(catNames, name)
	}
	sort.Strings(catNames)

	var parts []string
	for _, name := range catNames {
		members := categories[name]
		parts = append(parts, fmt.Sprintf("%s (%s)", name, strings.Join(members, ", ")))
	}

	return strings.Join(parts, " | ")
}

// ---------------------------------------------------------------------------
// Prompt rendering — generates markdown sections for main_agent_prompt.md
// ---------------------------------------------------------------------------

// RenderHotToolsList returns a markdown bullet list of hot tools for the
// agent prompt. Each bullet has the tool name bolded and its hint.
// Output is deterministic (sorted by tool name).
//
// Example output:
//
//	- **done** — signal you're finished (REQUIRED, call last)
//	- **reply** — generate and send a response (REQUIRED every turn)
func RenderHotToolsList() string {
	var lines []string
	for _, name := range hotTools {
		hint := hotToolHints[name]
		if hint == "" {
			// Fallback: use first sentence of the tool's description.
			if def, ok := toolDefs[name]; ok {
				hint = firstSentence(def.Function.Description)
			}
		}
		lines = append(lines, fmt.Sprintf("- **%s** — %s", name, hint))
	}
	return strings.Join(lines, "\n")
}

// RenderCategoryTable returns a markdown table of deferred tool categories
// for the agent prompt. Output is deterministic (sorted by category name).
//
// Example output:
//
//	| Category | Tools | When to use |
//	|---|---|---|
//	| **context** | get_weather, set_location | User asks about weather... |
func RenderCategoryTable() string {
	catNames := sortedCategoryNames()

	var lines []string
	lines = append(lines, "| Category | Tools | When to use |")
	lines = append(lines, "|---|---|---|")
	for _, name := range catNames {
		members := categories[name]
		hint := categoryHints[name]
		lines = append(lines, fmt.Sprintf("| **%s** | %s | %s |",
			name, strings.Join(members, ", "), hint))
	}
	return strings.Join(lines, "\n")
}

// sortedCategoryNames returns category names in sorted order.
func sortedCategoryNames() []string {
	names := make([]string, 0, len(categories))
	for name := range categories {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// firstSentence returns the text up to the first period+space or newline.
// Used as a fallback hint when a tool's YAML doesn't have a hint field.
func firstSentence(s string) string {
	if i := strings.Index(s, ". "); i >= 0 {
		return s[:i+1]
	}
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// RegisterDef adds a Go-defined tool to the registry. Used by agent/
// for tools still defined in Go code (reply, etc.) so there's one
// unified registry. Also adds to hotTools/categories if applicable.
func RegisterDef(def llm.ToolDef, hot bool, category string) {
	name := def.Function.Name
	toolDefs[name] = def

	if hot {
		// Avoid duplicates.
		for _, h := range hotTools {
			if h == name {
				goto skipHot
			}
		}
		hotTools = append(hotTools, name)
	skipHot:
	}

	if category != "" {
		members := categories[category]
		for _, m := range members {
			if m == name {
				goto skipCat
			}
		}
		categories[category] = append(categories[category], name)
	skipCat:
	}
}

// ExpandToolIdentity replaces {{her}} and {{user}} placeholders in a tool's
// description and parameter descriptions with the configured identity names.
// Tool defs are built at init() time (before config exists), so placeholders
// are resolved per-request when tools are served to the LLM.
func ExpandToolIdentity(t llm.ToolDef, cfg *config.Config) llm.ToolDef {
	t.Function.Description = cfg.ExpandPrompt(t.Function.Description)

	// Also expand in parameter descriptions (one level deep).
	// We need type assertions because Parameters is interface{}.
	params, ok := t.Function.Parameters.(map[string]interface{})
	if !ok {
		return t
	}
	props, ok := params["properties"].(map[string]interface{})
	if !ok {
		return t
	}

	// Check if any descriptions need expansion before copying.
	needsCopy := false
	for _, val := range props {
		if prop, ok := val.(map[string]interface{}); ok {
			if desc, ok := prop["description"].(string); ok {
				if cfg.ExpandPrompt(desc) != desc {
					needsCopy = true
					break
				}
			}
		}
	}
	if !needsCopy {
		return t
	}

	// Copy the maps to avoid mutating the shared registry.
	newParams := make(map[string]interface{}, len(params))
	for k, v := range params {
		newParams[k] = v
	}
	newProps := make(map[string]interface{}, len(props))
	for k, v := range props {
		newProps[k] = v
	}
	newParams["properties"] = newProps
	t.Function.Parameters = newParams

	for key, val := range newProps {
		if prop, ok := val.(map[string]interface{}); ok {
			if desc, ok := prop["description"].(string); ok {
				expanded := cfg.ExpandPrompt(desc)
				if expanded != desc {
					newProp := make(map[string]interface{}, len(prop))
					for k, v := range prop {
						newProp[k] = v
					}
					newProp["description"] = expanded
					newProps[key] = newProp
				}
			}
		}
	}
	return t
}

// HotToolDefs returns the always-loaded tools plus the use_tools meta-tool.
// This is what gets passed to ChatCompletionWithTools on the first iteration.
// The cfg parameter is used to expand {{her}}/{{user}} placeholders.
func HotToolDefs(cfg *config.Config) []llm.ToolDef {
	result := make([]llm.ToolDef, 0, len(hotTools)+1)
	for _, name := range hotTools {
		if t, ok := toolDefs[name]; ok {
			result = append(result, ExpandToolIdentity(t, cfg))
		}
	}
	// Add the use_tools meta-tool for loading deferred tools.
	result = append(result, UseToolsDef())
	return result
}

// LookupToolDefs resolves a mix of tool names and category names into
// full tool definitions. Unknown names are silently skipped.
func LookupToolDefs(names []string, cfg *config.Config) []llm.ToolDef {
	seen := make(map[string]bool)
	var result []llm.ToolDef

	for _, name := range names {
		// Check if it's a category first.
		if members, ok := categories[name]; ok {
			for _, member := range members {
				if !seen[member] {
					if t, ok := toolDefs[member]; ok {
						result = append(result, ExpandToolIdentity(t, cfg))
						seen[member] = true
					}
				}
			}
			continue
		}
		// Otherwise treat it as a tool name.
		if !seen[name] {
			if t, ok := toolDefs[name]; ok {
				result = append(result, ExpandToolIdentity(t, cfg))
				seen[name] = true
			}
		}
	}
	return result
}

// UseToolsDef returns the meta-tool that loads deferred tools on demand.
// Its description is dynamically generated from the loaded categories.
func UseToolsDef() llm.ToolDef {
	catDesc := CategoryDescription()
	return llm.ToolDef{
		Type: "function",
		Function: llm.ToolFunctionDef{
			Name: "use_tools",
			Description: fmt.Sprintf(
				"Load additional tools you need for this turn. Call BEFORE using a deferred tool. "+
					"Pass category names or individual tool names. Loaded tools stay available for "+
					"the rest of this turn.\n\nCategories: %s",
				catDesc,
			),
			Parameters: map[string]interface{}{
				"type": "object",
				"properties": map[string]interface{}{
					"tools": map[string]interface{}{
						"type":        "array",
						"items":       map[string]interface{}{"type": "string"},
						"description": "Category names to load. E.g., [\"search\"] or [\"vision\"].",
					},
				},
				"required": []string{"tools"},
			},
		},
	}
}
