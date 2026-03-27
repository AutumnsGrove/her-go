package skillkit

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestOutput verifies that Output writes valid JSON to stdout.
//
// Go testing pattern: we swap the package-level `stdout` variable
// with a bytes.Buffer to capture what Output writes, then inspect it.
// The defer restores the original — like a context manager in Python.
func TestOutput(t *testing.T) {
	tests := []struct {
		name  string
		input any
		want  string // expected JSON output (without trailing newline)
	}{
		{
			name:  "string map",
			input: map[string]string{"echo": "hello"},
			want:  `{"echo":"hello"}`,
		},
		{
			name:  "nested struct",
			input: struct{ N int }{N: 42},
			want:  `{"N":42}`,
		},
		{
			name:  "slice",
			input: []int{1, 2, 3},
			want:  `[1,2,3]`,
		},
		{
			name:  "html chars not escaped",
			input: map[string]string{"q": "a<b>c&d"},
			want:  `{"q":"a<b>c&d"}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			stdout = &buf
			defer func() { stdout = nil }()

			Output(tt.input)

			// Encoder.Encode adds a trailing newline, so we trim it
			got := bytes.TrimSpace(buf.Bytes())
			if string(got) != tt.want {
				t.Errorf("Output(%v) = %s, want %s", tt.input, got, tt.want)
			}
		})
	}
}

// TestOutputInvalidValue verifies that Output handles unencodable
// values by writing an error JSON object and exiting.
func TestOutputInvalidValue(t *testing.T) {
	var buf bytes.Buffer
	stdout = &buf
	defer func() { stdout = nil }()

	// Capture the exit call instead of actually exiting.
	var exitCode int
	exit = func(code int) { exitCode = code }
	defer func() { exit = nil }()

	// Channels can't be JSON-encoded — this should trigger the
	// error fallback path.
	Output(make(chan int))

	if exitCode != 1 {
		t.Errorf("expected exit code 1, got %d", exitCode)
	}

	// The output should contain an error field.
	output := buf.String()
	if !bytes.Contains(buf.Bytes(), []byte(`"error"`)) {
		t.Errorf("expected error JSON, got: %s", output)
	}
}

// TestError verifies that Error writes a JSON error and exits.
func TestError(t *testing.T) {
	var buf bytes.Buffer
	stdout = &buf
	defer func() { stdout = nil }()

	var exitCode int
	exit = func(code int) { exitCode = code }
	defer func() { exit = nil }()

	Error("something went wrong")

	if exitCode != 1 {
		t.Errorf("expected exit code 1, got %d", exitCode)
	}

	// Parse the output to verify the structure.
	var result struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(buf.Bytes()), &result); err != nil {
		t.Fatalf("output is not valid JSON: %s", buf.String())
	}
	if result.Error != "something went wrong" {
		t.Errorf("error message = %q, want %q", result.Error, "something went wrong")
	}
}

// TestLogf verifies that Logf writes to stderr with a trailing newline.
func TestLogf(t *testing.T) {
	tests := []struct {
		name   string
		format string
		args   []any
		want   string
	}{
		{
			name:   "simple message",
			format: "hello",
			want:   "hello\n",
		},
		{
			name:   "formatted message",
			format: "page %d of %d",
			args:   []any{2, 5},
			want:   "page 2 of 5\n",
		},
		{
			name:   "already has newline",
			format: "done\n",
			want:   "done\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			stderr = &buf
			defer func() { stderr = nil }()

			Logf(tt.format, tt.args...)

			if buf.String() != tt.want {
				t.Errorf("Logf(%q) wrote %q, want %q", tt.format, buf.String(), tt.want)
			}
		})
	}
}
