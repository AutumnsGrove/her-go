// Package skillkit provides shared utilities for her-go skills.
//
// Skills are standalone binaries that receive JSON on stdin, do work,
// and write JSON to stdout. This package handles the boilerplate:
// argument parsing, structured output, and HTTP requests.
//
// Think of it like a mini framework — similar to how Flask gives you
// request parsing and response formatting so you can focus on your
// route handler logic. Skillkit does the same for skill binaries.
package skillkit

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
)

// These are package-level variables so tests can swap them out.
// In Python you'd mock os.stdout — same idea, but Go doesn't have
// monkey-patching, so we use replaceable variables instead.
var (
	stdout io.Writer = os.Stdout
	stderr io.Writer = os.Stderr
	exit   func(int) = os.Exit
)

// Output writes a JSON-encoded value to stdout. This is how a skill
// returns its result to the harness. The harness reads stdout, parses
// the JSON, and passes it back to the agent.
//
// If encoding fails (e.g., you pass a channel or a func — types that
// can't be serialized to JSON), it writes an error and exits.
//
// Usage:
//
//	skillkit.Output(map[string]string{"echo": "hello"})
//	// stdout: {"echo":"hello"}
func Output(v any) {
	// json.NewEncoder writes directly to the writer — no intermediate
	// buffer like json.Marshal would create. More efficient, and it
	// adds a trailing newline automatically.
	enc := json.NewEncoder(stdout)

	// By default, Go's JSON encoder escapes <, >, and & for safe
	// embedding in HTML. We don't need that — skills return data,
	// not HTML. SetEscapeHTML(false) keeps the output clean.
	enc.SetEscapeHTML(false)

	if err := enc.Encode(v); err != nil {
		// If we can't even encode the result, something is deeply
		// wrong (like passing a channel). Write a fallback error
		// manually — we can't call Error() here because that would
		// also try to encode JSON and might fail the same way.
		fmt.Fprintf(stdout, `{"error":"failed to encode output: %s"}`+"\n", err)
		exit(1)
	}
}

// Error writes a JSON error object to stdout and terminates the skill.
// The harness checks for the "error" key to detect skill failures.
//
// This is a deliberate design choice: a skill that hits an error should
// stop immediately rather than producing partial or corrupt output.
// It's like Python's sys.exit() — you're done, bail out.
//
// Usage:
//
//	if apiKey == "" {
//	    skillkit.Error("TAVILY_API_KEY not set")
//	}
//	// stdout: {"error":"TAVILY_API_KEY not set"}
//	// process exits with code 1
func Error(msg string) {
	// Using an anonymous struct with a json tag. This is a common Go
	// pattern for one-off JSON shapes — no need to define a named type
	// when you only use it once. Same as a dict literal in Python.
	Output(struct {
		Error string `json:"error"`
	}{Error: msg})
	exit(1)
}

// Logf writes a formatted message to stderr. The harness captures
// stderr for debug logging, but it never reaches the agent's context
// window — so log freely without worrying about token costs.
//
// This is like print(..., file=sys.stderr) in Python.
//
// Usage:
//
//	skillkit.Logf("fetching page %d of %d", page, totalPages)
func Logf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	if !strings.HasSuffix(msg, "\n") {
		msg += "\n"
	}
	fmt.Fprint(stderr, msg)
}
