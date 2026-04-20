// Package classifier provides the quality-gate LLM classifier used across
// the codebase to validate memory writes and outgoing reply style.
//
// It lives in its own package (rather than inside agent/) so that agent/,
// tools/, memory/, and persona/ can all import it directly without circular
// dependencies. The classifier only depends on llm and memory — nothing
// higher up the stack.
//
// Primary entry points:
//
//	verdict := classifier.Check(llm, "fact", content, snippet)
//	msg := classifier.RejectionMessage(verdict)
package classifier

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"

	"her/llm"
	"her/memory"
)

// Verdict is the result of a classifier check on a proposed memory write
// or outgoing reply. Allowed=true means the write/reply is approved.
type Verdict struct {
	Allowed bool
	Type    string   // "SAVE", "PASS", "SPLIT", "LOW_VALUE", etc.
	Reason  string   // explanation from the classifier (may be empty)
	Rewrite string   // suggested rewrite for soft verdicts (may be empty)
	Splits  []string // sub-memory texts for SPLIT verdict (may be nil)
}

// ---------------------------------------------------------------------------
// YAML schema types
// ---------------------------------------------------------------------------

type classifierFile struct {
	Classifiers map[string]classifierDef `yaml:"classifiers"`
}

type classifierDef struct {
	Preamble   string       `yaml:"preamble"`
	WriteTypes []string     `yaml:"write_types"`
	Verdicts   []verdictDef `yaml:"verdicts"`
	Footer     string       `yaml:"footer"`
}

type verdictDef struct {
	Name        string        `yaml:"name"`
	Soft        bool          `yaml:"soft,omitempty"`
	Description string        `yaml:"description"`
	Examples    []string      `yaml:"examples,omitempty"`
	Note        string        `yaml:"note,omitempty"`
	Rejection   *rejectionDef `yaml:"rejection,omitempty"`
}

type rejectionDef struct {
	DefaultDetail string `yaml:"default_detail"`
	Suffix        string `yaml:"suffix"`
}

// ---------------------------------------------------------------------------
// Compiled state — built once at init(), immutable after that
// ---------------------------------------------------------------------------

type classifierState struct {
	// systemPrompts maps writeType ("fact", "reply", etc.) to the
	// pre-rendered system prompt string for that classifier.
	systemPrompts map[string]string

	// verdictNames lists all non-SAVE/non-PASS verdict names.
	// Used by parseResponse for prefix matching.
	verdictNames []string

	// rejections maps verdict name to its rejection definition.
	rejections map[string]*rejectionDef

	// softVerdicts tracks which verdict types suggest rewrites instead
	// of hard rejecting (set from `soft: true` in classifiers.yaml).
	softVerdicts map[string]bool
}

var state classifierState

//go:embed classifiers.yaml
var classifiersYAML []byte

// The system prompt template renders the numbered verdict list that the
// LLM sees. "add" is a custom func for 1-based numbering (Go templates
// don't have built-in arithmetic).
var systemPromptTmpl = template.Must(template.New("prompt").Funcs(template.FuncMap{
	"add": func(a, b int) int { return a + b },
}).Parse(`{{ .Preamble }}

Check in this order:
{{ range $i, $v := .Verdicts }}
{{ add $i 1 }}. {{ $v.Name }}{{ if $v.Description }} — {{ $v.Description }}{{ end }}
{{- if and $v.Examples (ne $v.Name "SAVE") }}
   Examples{{ if ne $v.Name "SAVE" }} of {{ $v.Name }}{{ end }}:
{{- range $v.Examples }}
   - {{ . }}
{{- end }}
{{- end }}
{{- if $v.Note }}
   Note: {{ $v.Note }}
{{- end }}
{{ end }}
{{ .Footer }}`))

func init() {
	var file classifierFile
	if err := yaml.Unmarshal(classifiersYAML, &file); err != nil {
		panic(fmt.Sprintf("classifier: failed to parse classifiers.yaml: %v", err))
	}

	s := classifierState{
		systemPrompts: make(map[string]string),
		rejections:    make(map[string]*rejectionDef),
		softVerdicts:  make(map[string]bool),
	}

	// Track verdict names seen so duplicates (e.g., FICTIONAL in both
	// fact and receipt classifiers) are only added once.
	seen := make(map[string]bool)

	for _, def := range file.Classifiers {
		var buf strings.Builder
		if err := systemPromptTmpl.Execute(&buf, def); err != nil {
			panic(fmt.Sprintf("classifier: failed to render prompt template: %v", err))
		}
		prompt := buf.String()

		for _, wt := range def.WriteTypes {
			s.systemPrompts[wt] = prompt
		}

		for i := range def.Verdicts {
			v := &def.Verdicts[i]
			if v.Name == "SAVE" || v.Name == "PASS" {
				continue
			}
			if !seen[v.Name] {
				s.verdictNames = append(s.verdictNames, v.Name)
				seen[v.Name] = true
			}
			if v.Rejection != nil {
				s.rejections[v.Name] = v.Rejection
			}
			if v.Soft {
				s.softVerdicts[v.Name] = true
			}
		}
	}

	state = s
}

// ---------------------------------------------------------------------------
// Public API
// ---------------------------------------------------------------------------

// Check asks the classifier LLM whether a proposed write or reply should be
// allowed. Returns Verdict{Allowed: true} on any error (fail-open).
//
// writeType: "fact", "self_fact", "reply", "dream", etc. — must match a
// classifier defined in classifiers.yaml.
// content: the text being evaluated.
// snippet: recent conversation messages for context (may be nil).
func Check(
	classifierLLM *llm.Client,
	writeType string,
	content string,
	snippet []memory.Message,
) Verdict {
	if classifierLLM == nil {
		return Verdict{Allowed: true, Type: "SAVE"}
	}

	// Build conversation context from the snippet. Truncate each message
	// to 200 chars — enough for the classifier to distinguish real-life
	// from fictional without inflating token counts.
	const maxSnippetChars = 200
	var contextLines []string
	for _, msg := range snippet {
		text := msg.ContentScrubbed
		if text == "" {
			text = msg.ContentRaw
		}
		if len(text) > maxSnippetChars {
			text = text[:maxSnippetChars] + "..."
		}
		contextLines = append(contextLines, fmt.Sprintf("%s: %s", msg.Role, text))
	}
	contextStr := strings.Join(contextLines, "\n")

	// Unknown write type → don't block the write.
	systemPrompt, ok := state.systemPrompts[writeType]
	if !ok {
		return Verdict{Allowed: true, Type: "SAVE"}
	}

	userPrompt := fmt.Sprintf(
		"Conversation context:\n%s\n\nProposed %s to save:\n%s",
		contextStr, writeType, content,
	)

	messages := []llm.ChatMessage{
		{Role: "system", Content: systemPrompt},
		{Role: "user", Content: userPrompt},
	}

	resp, err := classifierLLM.ChatCompletion(messages)
	if err != nil {
		// Fail-open: a missing fact is annoying; blocking ALL memory
		// writes because the classifier is down would break the bot.
		return Verdict{Allowed: true, Type: "SAVE"}
	}

	return parseResponse(resp.Content)
}

// RejectionMessage builds the agent-facing string when a write is rejected.
// The detail text comes from classifiers.yaml; soft verdicts with a rewrite
// suggestion get a different message encouraging the agent to use it.
func RejectionMessage(v Verdict) string {
	if v.Rewrite != "" {
		return fmt.Sprintf("suggestion: this fact mixes real and fictional/inferred content. Try saving this instead: %q", v.Rewrite)
	}

	rej, ok := state.rejections[v.Type]
	if !ok {
		return fmt.Sprintf("rejected by classifier: %s", v.Reason)
	}

	detail := rej.DefaultDetail
	if v.Reason != "" {
		detail = v.Reason
	}
	return fmt.Sprintf("rejected: %s. %s", detail, rej.Suffix)
}

// ---------------------------------------------------------------------------
// Internal helpers
// ---------------------------------------------------------------------------

func parseResponse(response string) Verdict {
	// Split into first line and everything after it.
	// Most verdicts live on the first line only. SPLIT is the exception —
	// it carries a JSON array of sub-memories on subsequent lines.
	response = strings.TrimSpace(response)
	line := response
	remainder := ""
	if idx := strings.IndexAny(response, "\n\r"); idx >= 0 {
		line = strings.TrimSpace(response[:idx])
		remainder = strings.TrimSpace(response[idx:])
	}
	upper := strings.ToUpper(line)

	if strings.HasPrefix(upper, "SAVE") {
		return Verdict{Allowed: true, Type: "SAVE"}
	}
	if strings.HasPrefix(upper, "PASS") {
		return Verdict{Allowed: true, Type: "PASS"}
	}
	if strings.HasPrefix(upper, "SAFE") {
		return Verdict{Allowed: true, Type: "SAFE"}
	}

	// SPLIT: allowed (we're saving, just atomizing). Parse the JSON array
	// of sub-memories from the remainder. If parsing fails or returns fewer
	// than 2 items, the caller falls through to save the original.
	if strings.HasPrefix(upper, "SPLIT") {
		v := Verdict{Allowed: true, Type: "SPLIT", Reason: extractReason(line)}
		if remainder != "" {
			var splits []string
			if err := json.Unmarshal([]byte(remainder), &splits); err == nil && len(splits) >= 2 {
				v.Splits = splits
			}
		}
		return v
	}

	for _, name := range state.verdictNames {
		if strings.HasPrefix(upper, name) {
			v := Verdict{Allowed: false, Type: name}
			rewrite := extractRewrite(line, name)
			if rewrite != "" && state.softVerdicts[name] {
				v.Rewrite = rewrite
			}
			v.Reason = extractReason(line)
			return v
		}
	}

	// Unparseable → fail-open.
	return Verdict{Allowed: true, Type: "SAVE"}
}

func extractReason(line string) string {
	parts := strings.SplitN(line, " ", 2)
	if len(parts) < 2 {
		return ""
	}
	return strings.TrimLeft(strings.TrimSpace(parts[1]), "—-–: ")
}

func extractRewrite(line, verdictName string) string {
	upper := strings.ToUpper(line)
	idx := strings.Index(upper, "REWRITE:")
	if idx < 0 {
		return ""
	}
	after := strings.TrimSpace(line[idx+len("REWRITE:"):])
	if len(after) >= 2 && after[0] == '"' && after[len(after)-1] == '"' {
		after = after[1 : len(after)-1]
	}
	return after
}
