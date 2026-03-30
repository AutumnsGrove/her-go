package agent

import (
	// embed is imported for its side effect: the //go:embed directive below
	// bakes classifiers.yaml into the binary at compile time. No import
	// alias needed — the blank identifier tells Go "I need this package
	// but I'm not calling any of its functions directly."
	_ "embed"
	"fmt"
	"strings"
	"text/template"

	"gopkg.in/yaml.v3"

	"her/llm"
	"her/memory"
	"her/tools"
)

// ClassifyVerdict is an alias for tools.ClassifyVerdict. It was originally
// defined here but moved to the tools package so handler code in tools/
// can reference it without creating a circular import. The agent still
// uses the type by its tools-package name.
type ClassifyVerdict = tools.ClassifyVerdict

// ---------------------------------------------------------------------------
// YAML schema types
// ---------------------------------------------------------------------------

// classifierFile is the top-level YAML structure. It maps classifier
// names (fact, mood, receipt) to their definitions.
type classifierFile struct {
	Classifiers map[string]classifierDef `yaml:"classifiers"`
}

// classifierDef defines one classifier (fact, mood, or receipt).
// WriteTypes lists which writeType values route to this classifier
// (e.g., "fact" and "self_fact" both route to the fact classifier).
type classifierDef struct {
	Preamble   string       `yaml:"preamble"`
	WriteTypes []string     `yaml:"write_types"`
	Verdicts   []verdictDef `yaml:"verdicts"`
	Footer     string       `yaml:"footer"`
}

// verdictDef defines a single verdict within a classifier. Order in the
// YAML list = priority order (first match wins). SAVE verdicts have no
// Rejection field.
type verdictDef struct {
	Name        string        `yaml:"name"`
	Description string        `yaml:"description"`
	Examples    []string      `yaml:"examples,omitempty"`
	Note        string        `yaml:"note,omitempty"`
	Rejection   *rejectionDef `yaml:"rejection,omitempty"`
}

// rejectionDef defines the rejection message for a verdict.
// DefaultDetail is the fallback when the classifier doesn't provide a reason.
// Suffix is the actionable guidance appended after the detail.
type rejectionDef struct {
	DefaultDetail string `yaml:"default_detail"`
	Suffix        string `yaml:"suffix"`
}

// ---------------------------------------------------------------------------
// Compiled state — built once at init(), immutable after that
// ---------------------------------------------------------------------------

// classifierState holds everything derived from the YAML at startup:
// pre-rendered system prompts, verdict name lists for parsing, and
// rejection message data for each verdict type.
type classifierState struct {
	// systemPrompts maps writeType ("fact", "mood", etc.) to the
	// pre-rendered system prompt string for that classifier.
	systemPrompts map[string]string

	// verdictNames lists all non-SAVE verdict names across all
	// classifiers. Used by parseClassifierResponse for matching.
	verdictNames []string

	// rejections maps verdict name to its rejection definition.
	// Used by rejectionMessage to build the response string.
	rejections map[string]*rejectionDef
}

var classifiers classifierState

// classifiers.yaml is embedded into the binary at compile time.
// This means no runtime file I/O, no path issues when running the
// binary from a different directory. Changes require a rebuild,
// which is already the expectation ("add YAML block + restart").
//
//go:embed classifiers.yaml
var classifiersYAML []byte

// The system prompt template. It takes a classifierDef and renders the
// numbered verdict list that the LLM sees. Each verdict gets its index,
// name, description, examples, and note. The template uses {{- to trim
// whitespace so the output matches the original hand-written prompts.
//
// "add" is a custom template function: Go templates don't have arithmetic
// built in, so we register a simple func(a, b int) int { return a + b }
// to generate 1-based numbering from 0-based indices.
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

	state := classifierState{
		systemPrompts: make(map[string]string),
		rejections:    make(map[string]*rejectionDef),
	}

	// Track verdict names we've already seen so we don't add duplicates.
	// FICTIONAL appears in both fact and receipt classifiers, but the
	// parser only needs it once.
	seen := make(map[string]bool)

	for _, def := range file.Classifiers {
		// Render the system prompt once and cache it.
		var buf strings.Builder
		if err := systemPromptTmpl.Execute(&buf, def); err != nil {
			panic(fmt.Sprintf("classifier: failed to render prompt template: %v", err))
		}
		prompt := buf.String()

		// Map each writeType to this classifier's rendered prompt.
		for _, wt := range def.WriteTypes {
			state.systemPrompts[wt] = prompt
		}

		// Collect verdict names and rejection definitions.
		for i := range def.Verdicts {
			v := &def.Verdicts[i]
			if v.Name == "SAVE" {
				continue
			}
			if !seen[v.Name] {
				state.verdictNames = append(state.verdictNames, v.Name)
				seen[v.Name] = true
			}
			if v.Rejection != nil {
				state.rejections[v.Name] = v.Rejection
			}
		}
	}

	classifiers = state
}

// ---------------------------------------------------------------------------
// Public API — signatures unchanged from before the YAML migration
// ---------------------------------------------------------------------------

// classifyMemoryWrite asks the classifier LLM whether a proposed memory
// write should be saved to the database. It checks for multiple quality
// issues: fictional content, low-value facts, transient moods stored as
// permanent facts, and inferred-not-stated information.
//
// This is the single entry point for all classifier checks. It builds
// the right prompt based on writeType, calls the classifier, and parses
// the response. On ANY error (nil client, LLM failure, unparseable
// response), it returns Allowed=true — fail-open, because a missing
// fact is less harmful than a broken memory system.
//
// writeType: "fact", "self_fact", "mood", or "receipt"
// content: the proposed text (fact text, mood note, or receipt summary)
// snippet: last few messages of conversation for context
func classifyMemoryWrite(
	classifierLLM *llm.Client,
	writeType string,
	content string,
	snippet []memory.Message,
) ClassifyVerdict {
	// Nil-safe: no classifier configured → pass through.
	// This makes the classifier purely opt-in via config.yaml.
	if classifierLLM == nil {
		return ClassifyVerdict{Allowed: true, Type: "SAVE"}
	}

	// Build conversation context from the message snippet.
	// We show the classifier the last few messages so it can tell whether
	// "I got a new sword" is the user talking about a game or real life.
	var contextLines []string
	for _, msg := range snippet {
		// Prefer scrubbed content (PII-safe), fall back to raw.
		text := msg.ContentScrubbed
		if text == "" {
			text = msg.ContentRaw
		}
		contextLines = append(contextLines, fmt.Sprintf("%s: %s", msg.Role, text))
	}
	contextStr := strings.Join(contextLines, "\n")

	// Look up the pre-rendered system prompt for this writeType.
	// Unknown write types → don't block the write.
	systemPrompt, ok := classifiers.systemPrompts[writeType]
	if !ok {
		return ClassifyVerdict{Allowed: true, Type: "SAVE"}
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
		// Fail-open: if the classifier is down, let the write through.
		// A missing fact is annoying; blocking ALL memory writes because
		// a safety check is offline would make the bot feel broken.
		log.Warn("classifier LLM failed, allowing write (fail-open)", "err", err, "type", writeType)
		return ClassifyVerdict{Allowed: true, Type: "SAVE"}
	}

	verdict := parseClassifierResponse(resp.Content)
	if !verdict.Allowed {
		log.Info("classifier rejected write", "type", writeType, "verdict", verdict.Type, "reason", verdict.Reason, "content", content)
	}
	return verdict
}

// parseClassifierResponse extracts the verdict from the classifier's
// plain-text response. The first word is the verdict type, everything
// after it on the same line is the reason/explanation.
//
// We use simple string prefix matching rather than JSON parsing because
// small models (Haiku-class) are more reliable with free-form text than
// structured output. The verdict names are loaded from classifiers.yaml
// so adding a new verdict there automatically makes the parser recognize it.
func parseClassifierResponse(response string) ClassifyVerdict {
	line := strings.TrimSpace(response)
	// Take only the first line — the model might add extra explanation.
	if idx := strings.IndexAny(line, "\n\r"); idx >= 0 {
		line = strings.TrimSpace(line[:idx])
	}
	upper := strings.ToUpper(line)

	// --- Allowed verdicts ---
	if strings.HasPrefix(upper, "SAVE") {
		return ClassifyVerdict{Allowed: true, Type: "SAVE"}
	}

	// --- Rejected verdicts ---
	// Loop through all known verdict names (loaded from YAML).
	// Order doesn't matter here — the LLM already picked one verdict,
	// we're just matching the response text to a known name.
	for _, name := range classifiers.verdictNames {
		if strings.HasPrefix(upper, name) {
			return ClassifyVerdict{Allowed: false, Type: name, Reason: extractReason(line)}
		}
	}

	// Unparseable response → fail-open.
	log.Warn("classifier returned unparseable response, allowing write", "response", response)
	return ClassifyVerdict{Allowed: true, Type: "SAVE"}
}

// extractReason pulls the explanation text after the verdict keyword.
// "FICTIONAL — this is about a game character" → "this is about a game character"
// "LOW_VALUE" → "" (no explanation given)
func extractReason(line string) string {
	// Strip the verdict keyword (first word).
	parts := strings.SplitN(line, " ", 2)
	if len(parts) < 2 {
		return ""
	}
	reason := strings.TrimSpace(parts[1])
	// Strip leading punctuation that models sometimes add (—, -, :).
	reason = strings.TrimLeft(reason, "—-–: ")
	return reason
}

// rejectionMessage builds the string that gets returned to the agent
// when the classifier rejects a write. The message is tailored to the
// verdict type so the agent knows what to do differently — not just
// "rejected" but "rejected, and here's the right action to take."
//
// The detail and suffix text come from classifiers.yaml. If the
// classifier provided a reason, it replaces the default detail.
func rejectionMessage(verdict ClassifyVerdict) string {
	rej, ok := classifiers.rejections[verdict.Type]
	if !ok {
		return fmt.Sprintf("rejected by classifier: %s", verdict.Reason)
	}

	detail := rej.DefaultDetail
	if verdict.Reason != "" {
		detail = verdict.Reason
	}
	return fmt.Sprintf("rejected: %s. %s", detail, rej.Suffix)
}
