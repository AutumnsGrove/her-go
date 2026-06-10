package workeragent

import (
	"os"
	"path/filepath"
	"testing"

	"her/llm"
)

func TestExtractTitle(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "report.md")

	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"normal heading", "# My Report\n\nBody text.", "My Report"},
		{"heading with spaces", "#   Spaced Title  \n\nBody.", "Spaced Title"},
		{"no heading", "Just some text.\nMore text.", ""},
		{"h2 not h1", "## Subheading\n\nBody.", ""},
		{"heading after blank lines", "\n\n# Late Heading\n", "Late Heading"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			os.WriteFile(path, []byte(tt.content), 0644)
			got := extractTitle(path)
			if got != tt.want {
				t.Errorf("extractTitle() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestExtractDoneSummary(t *testing.T) {
	messages := []llm.ChatMessage{
		{Role: "user", Content: "do research"},
		{Role: "assistant", Content: "", ToolCalls: []llm.ToolCall{
			{Function: llm.FunctionCall{Name: "think", Arguments: `{"thought":"planning"}`}},
		}},
		{Role: "assistant", Content: "", ToolCalls: []llm.ToolCall{
			{Function: llm.FunctionCall{Name: "done", Arguments: `{"summary":"Wrote a report on Go testing."}`}},
		}},
	}

	got := extractDoneSummary(messages)
	if got != "Wrote a report on Go testing." {
		t.Errorf("extractDoneSummary() = %q", got)
	}
}

func TestExtractDoneSummary_NoDone(t *testing.T) {
	messages := []llm.ChatMessage{
		{Role: "user", Content: "hello"},
		{Role: "assistant", Content: "hi there"},
	}

	got := extractDoneSummary(messages)
	if got != "" {
		t.Errorf("expected empty, got %q", got)
	}
}

func TestBuildWorkerInstruction(t *testing.T) {
	input := WorkerInput{
		TaskType:    "research",
		Instruction: "Deep dive on htmx",
		Payload:     map[string]string{"depth": "thorough"},
	}

	result := buildWorkerInstruction(input)
	if result == "" {
		t.Fatal("expected non-empty instruction")
	}

	// Should contain the task type, instruction, and payload.
	for _, want := range []string{"research", "Deep dive on htmx", "depth: thorough"} {
		if !contains(result, want) {
			t.Errorf("instruction missing %q:\n%s", want, result)
		}
	}
}

func contains(s, substr string) bool {
	return len(s) >= len(substr) && searchString(s, substr)
}

func searchString(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
