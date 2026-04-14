// Package tools — trace formatting for the agent thinking trace.
//
// Each tool declares a trace: block in its tool.yaml that specifies
// how to format that tool's trace line (emoji, which args to show,
// format template). The FormatTrace function replaces the 20-case
// switch statement that used to live in agent.go.
//
// Templates use Go's text/template syntax with custom functions:
//   - escape: HTML-escape for Telegram (& < >)
//   - truncate N: cut string to N chars with "..." suffix
//
// Example: {{.thought | escape}} or {{.Result | truncate 80 | escape}}
package tools

import (
	"encoding/json"
	"fmt"
	"strings"
	"text/template"
)

// ---------------------------------------------------------------------------
// YAML schema for trace specifications
// ---------------------------------------------------------------------------

// traceSpec defines how a tool's trace line is formatted. Parsed from
// the trace: block in tool.yaml.
type traceSpec struct {
	Emoji    string            `yaml:"emoji"`
	Format   string            `yaml:"format,omitempty"`   // Go template string
	OnReject *traceRejectSpec  `yaml:"on_reject,omitempty"`
}

// traceRejectSpec defines alternate formatting when the result indicates
// rejection (e.g., classifier or style gate rejected a memory write).
type traceRejectSpec struct {
	Prefix string `yaml:"prefix"`          // result prefix to match (e.g., "rejected:")
	Emoji  string `yaml:"emoji"`           // override emoji for rejections
	Format string `yaml:"format,omitempty"` // Go template for rejection line
}

// compiledTrace holds a parsed trace spec with pre-compiled templates.
type compiledTrace struct {
	emoji    string
	tmpl     *template.Template // nil if no format (just emoji + name)
	onReject *compiledReject
}

// compiledReject holds the pre-compiled rejection template.
type compiledReject struct {
	prefix string
	emoji  string
	tmpl   *template.Template
}

// traceRegistry maps tool name → compiled trace spec.
var traceRegistry = map[string]*compiledTrace{}

// traceFuncs are the custom functions available in trace templates.
// - escape: HTML-escapes for Telegram (& < >)
// - truncate: cuts a string to N chars, appending "..." if truncated
// - default: returns a fallback when the piped value is missing/empty
//
// In templates, pipe syntax makes these readable:
//   {{.thought | escape}}
//   {{.Result | truncate 80 | escape}}
//   {{.repo | default "repos"}}
//
// Go templates pipe the value as the LAST argument, so truncate takes
// (maxLen int, s string) and default takes (fallback, value) — the piped
// value fills the last parameter in both cases.
var traceFuncs = template.FuncMap{
	"escape": func(s string) string {
		s = strings.ReplaceAll(s, "&", "&amp;")
		s = strings.ReplaceAll(s, "<", "&lt;")
		s = strings.ReplaceAll(s, ">", "&gt;")
		return s
	},
	"truncate": func(n int, s string) string {
		s = strings.ReplaceAll(s, "\n", " ")
		if len(s) <= n {
			return s
		}
		return s[:n] + "..."
	},
	// default returns fallback when value is nil or an empty string.
	// Mirrors Sprig's `default` so trace templates can write
	// {{.repo | default "repos"}} without pulling in the full library.
	"default": func(fallback, value interface{}) interface{} {
		if value == nil {
			return fallback
		}
		if s, ok := value.(string); ok && s == "" {
			return fallback
		}
		return value
	},
}

// compileTraceSpec parses a traceSpec into a compiledTrace with
// pre-compiled templates. Called at init time for each tool.
func compileTraceSpec(name string, spec traceSpec) *compiledTrace {
	ct := &compiledTrace{
		emoji: spec.Emoji,
	}

	if spec.Format != "" {
		tmpl, err := template.New(name).Funcs(traceFuncs).Parse(spec.Format)
		if err != nil {
			// Bad template in a per-tool YAML — log and degrade gracefully.
			// FormatTrace will fall back to the generic "🔧 name → result" format.
			log.Warn("tools: bad trace template, using generic fallback", "tool", name, "err", err)
		} else {
			ct.tmpl = tmpl
		}
	}

	if spec.OnReject != nil {
		cr := &compiledReject{
			prefix: spec.OnReject.Prefix,
			emoji:  spec.OnReject.Emoji,
		}
		if spec.OnReject.Format != "" {
			tmpl, err := template.New(name + "_reject").Funcs(traceFuncs).Parse(spec.OnReject.Format)
			if err != nil {
				// Bad reject template — skip the reject spec entirely.
				// The tool will still get its normal trace format on rejection.
				log.Warn("tools: bad reject trace template, disabling reject format", "tool", name, "err", err)
				return ct
			}
			cr.tmpl = tmpl
		}
		ct.onReject = cr
	}

	return ct
}

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

// FormatTrace builds an HTML-formatted trace line for a tool call.
// This replaces the formatTraceLine switch in agent.go.
//
// Each tool's trace spec comes from its tool.yaml. Tools without a
// trace spec get the generic default format.
func FormatTrace(toolName, argsJSON, result string) string {
	ct, ok := traceRegistry[toolName]
	if !ok {
		// No trace spec — use generic default.
		return fmt.Sprintf("🔧 <b>%s:</b> → %s", escapeHTMLTrace(toolName), escapeHTMLTrace(truncateTrace(result, 80)))
	}

	// Check for rejection first.
	if ct.onReject != nil && strings.HasPrefix(result, ct.onReject.prefix) {
		return renderTrace(ct.onReject.emoji, toolName, ct.onReject.tmpl, argsJSON, result)
	}

	return renderTrace(ct.emoji, toolName, ct.tmpl, argsJSON, result)
}

// renderTrace formats a trace line using the given emoji, tool name,
// and optional template. If tmpl is nil, just shows emoji + name.
func renderTrace(emoji, toolName string, tmpl *template.Template, argsJSON, result string) string {
	if tmpl == nil {
		// No template — just emoji + bold name.
		return fmt.Sprintf("%s <b>%s</b>", emoji, escapeHTMLTrace(toolName))
	}

	// Build template data: merge parsed JSON args with Result.
	data := map[string]interface{}{
		"Result": result,
	}
	if argsJSON != "" {
		var args map[string]interface{}
		if err := json.Unmarshal([]byte(argsJSON), &args); err == nil {
			for k, v := range args {
				// Convert numeric types to their string representation
				// for consistent template rendering.
				switch val := v.(type) {
				case float64:
					// Check if it's actually an integer.
					if val == float64(int64(val)) {
						data[k] = int64(val)
					} else {
						data[k] = val
					}
				default:
					data[k] = v
				}
			}
		}
	}

	var buf strings.Builder
	if err := tmpl.Execute(&buf, data); err != nil {
		// Template execution failed — fall back to generic.
		return fmt.Sprintf("%s <b>%s:</b> (trace error: %v)", emoji, escapeHTMLTrace(toolName), err)
	}

	return fmt.Sprintf("%s <b>%s:</b> %s", emoji, escapeHTMLTrace(toolName), buf.String())
}

// escapeHTMLTrace escapes HTML for Telegram. Same logic as agent's escapeHTML
// but defined here to avoid importing agent.
func escapeHTMLTrace(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

// truncateTrace shortens a string, collapsing newlines.
func truncateTrace(s string, maxLen int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
