package skillkit

import (
	"bytes"
	"strings"
	"testing"
)

// testArgs is a sample struct used across all ParseArgs tests.
// It exercises every supported field type: string, int, bool, float64.
type testArgs struct {
	Query   string  `json:"query" flag:"query" desc:"Search query"`
	Limit   int     `json:"limit" flag:"limit" desc:"Max results" default:"5"`
	Verbose bool    `json:"verbose" flag:"verbose" desc:"Verbose output"`
	Score   float64 `json:"score" flag:"score" desc:"Min score" default:"0.8"`
}

// saveAndRestore captures the current package vars and returns a cleanup
// function that restores them. Call it at the top of each test with defer.
//
// This is a helper pattern you'll see in Go tests when package-level
// state needs to be swapped — similar to unittest.mock.patch in Python,
// but manual. The defer ensures cleanup even if the test panics.
func saveAndRestore(t *testing.T) func() {
	t.Helper()
	origStdin := stdinReader
	origPipe := isStdinPipe
	origArgs := osArgs
	origStdout := stdout
	origExit := exit
	return func() {
		stdinReader = origStdin
		isStdinPipe = origPipe
		osArgs = origArgs
		stdout = origStdout
		exit = origExit
	}
}

// TestParseArgsStdinJSON verifies that ParseArgs reads JSON from stdin
// when input is piped. This is the primary path — the harness always
// pipes JSON to the skill binary.
func TestParseArgsStdinJSON(t *testing.T) {
	tests := []struct {
		name      string
		input     string
		wantQuery string
		wantLimit int
		wantBool  bool
		wantScore float64
	}{
		{
			name:      "all fields",
			input:     `{"query":"cats","limit":10,"verbose":true,"score":0.95}`,
			wantQuery: "cats",
			wantLimit: 10,
			wantBool:  true,
			wantScore: 0.95,
		},
		{
			name:      "partial fields use zero values",
			input:     `{"query":"dogs"}`,
			wantQuery: "dogs",
			wantLimit: 0, // Go zero value for int
			wantBool:  false,
			wantScore: 0.0,
		},
		{
			name:      "empty object",
			input:     `{}`,
			wantQuery: "",
			wantLimit: 0,
			wantBool:  false,
			wantScore: 0.0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer saveAndRestore(t)()

			// Simulate piped stdin with JSON content.
			stdinReader = strings.NewReader(tt.input)
			isStdinPipe = func() bool { return true }

			var args testArgs
			ParseArgs(&args)

			if args.Query != tt.wantQuery {
				t.Errorf("Query = %q, want %q", args.Query, tt.wantQuery)
			}
			if args.Limit != tt.wantLimit {
				t.Errorf("Limit = %d, want %d", args.Limit, tt.wantLimit)
			}
			if args.Verbose != tt.wantBool {
				t.Errorf("Verbose = %v, want %v", args.Verbose, tt.wantBool)
			}
			if args.Score != tt.wantScore {
				t.Errorf("Score = %f, want %f", args.Score, tt.wantScore)
			}
		})
	}
}

// TestParseArgsCLIFlags verifies that ParseArgs falls back to CLI flags
// when stdin is a terminal (not piped). This path is for manual testing.
func TestParseArgsCLIFlags(t *testing.T) {
	tests := []struct {
		name      string
		flags     []string
		wantQuery string
		wantLimit int
		wantBool  bool
		wantScore float64
	}{
		{
			name:      "all flags",
			flags:     []string{"--query", "cats", "--limit", "10", "--verbose", "--score", "0.95"},
			wantQuery: "cats",
			wantLimit: 10,
			wantBool:  true,
			wantScore: 0.95,
		},
		{
			name:      "defaults from tags",
			flags:     []string{"--query", "dogs"},
			wantQuery: "dogs",
			wantLimit: 5, // from default:"5" tag
			wantBool:  false,
			wantScore: 0.8, // from default:"0.8" tag
		},
		{
			name:      "no flags uses all defaults",
			flags:     []string{},
			wantQuery: "", // no default tag, so empty string
			wantLimit: 5,
			wantBool:  false,
			wantScore: 0.8,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			defer saveAndRestore(t)()

			// Simulate terminal stdin (no pipe).
			isStdinPipe = func() bool { return false }
			osArgs = func() []string { return tt.flags }

			var args testArgs
			ParseArgs(&args)

			if args.Query != tt.wantQuery {
				t.Errorf("Query = %q, want %q", args.Query, tt.wantQuery)
			}
			if args.Limit != tt.wantLimit {
				t.Errorf("Limit = %d, want %d", args.Limit, tt.wantLimit)
			}
			if args.Verbose != tt.wantBool {
				t.Errorf("Verbose = %v, want %v", args.Verbose, tt.wantBool)
			}
			if args.Score != tt.wantScore {
				t.Errorf("Score = %f, want %f", args.Score, tt.wantScore)
			}
		})
	}
}

// TestParseArgsEmptyPipeFallsBackToFlags verifies that an empty pipe
// (stdin is piped but no data) falls back to CLI flag parsing.
func TestParseArgsEmptyPipeFallsBackToFlags(t *testing.T) {
	defer saveAndRestore(t)()

	// Pipe exists but is empty.
	stdinReader = strings.NewReader("")
	isStdinPipe = func() bool { return true }
	osArgs = func() []string { return []string{"--query", "fallback"} }

	var args testArgs
	ParseArgs(&args)

	if args.Query != "fallback" {
		t.Errorf("Query = %q, want %q (should fall back to flags)", args.Query, "fallback")
	}
	if args.Limit != 5 {
		t.Errorf("Limit = %d, want 5 (default from tag)", args.Limit)
	}
}

// TestParseArgsInvalidJSON verifies that malformed JSON causes an error
// exit rather than silently producing garbage.
func TestParseArgsInvalidJSON(t *testing.T) {
	defer saveAndRestore(t)()

	stdinReader = strings.NewReader(`{not valid json}`)
	isStdinPipe = func() bool { return true }

	// Capture stdout and exit to verify error behavior.
	var buf bytes.Buffer
	stdout = &buf
	var exitCode int
	exit = func(code int) { exitCode = code }

	var args testArgs
	ParseArgs(&args)

	if exitCode != 1 {
		t.Errorf("expected exit code 1, got %d", exitCode)
	}
	if !bytes.Contains(buf.Bytes(), []byte("invalid input JSON")) {
		t.Errorf("expected error about invalid JSON, got: %s", buf.String())
	}
}

// TestParseArgsNonPointerPanics verifies that passing a non-pointer
// (a programming mistake) produces a clear error.
func TestParseArgsNonPointer(t *testing.T) {
	defer saveAndRestore(t)()

	var buf bytes.Buffer
	stdout = &buf
	var exitCode int
	exit = func(code int) { exitCode = code }

	// Passing a struct value instead of a pointer — skill author mistake.
	var args testArgs
	ParseArgs(args) // note: no & — this is the bug

	if exitCode != 1 {
		t.Errorf("expected exit code 1, got %d", exitCode)
	}
	if !bytes.Contains(buf.Bytes(), []byte("must be a pointer")) {
		t.Errorf("expected pointer error, got: %s", buf.String())
	}
}
