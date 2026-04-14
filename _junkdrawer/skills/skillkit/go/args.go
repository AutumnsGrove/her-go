package skillkit

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"reflect"
	"strconv"
)

// These are replaceable for testing — same pattern as output.go.
// In production they point at the real stdin and os.Args; tests can
// swap them to simulate piped JSON or CLI flags without touching the OS.
var (
	stdinReader io.Reader       = os.Stdin
	isStdinPipe func() bool     = defaultIsStdinPipe
	osArgs      func() []string = func() []string { return os.Args[1:] }
)

// defaultIsStdinPipe checks whether stdin is a pipe (data being fed in)
// rather than a terminal (human typing). This is how we decide whether
// to read JSON or parse CLI flags.
//
// In Python you'd check sys.stdin.isatty(). In Go, we stat the file
// descriptor and look at the mode bits. ModeCharDevice means "this is
// a terminal" — if that bit is NOT set, something is piping data to us.
func defaultIsStdinPipe() bool {
	info, err := os.Stdin.Stat()
	if err != nil {
		return false
	}
	return info.Mode()&os.ModeCharDevice == 0
}

// ParseArgs populates a struct from either stdin JSON or CLI flags.
//
// When the skill harness runs a skill, it pipes a JSON object to stdin.
// When a developer tests manually from the terminal, they pass CLI flags.
// ParseArgs handles both transparently — the skill code doesn't care
// which one is being used.
//
// The struct fields use tags to configure both modes:
//
//	type Args struct {
//	    Query string `json:"query" flag:"query" desc:"Search query"`
//	    Limit int    `json:"limit" flag:"limit" desc:"Max results" default:"5"`
//	}
//
// Tags:
//   - json:    key name for stdin JSON (standard encoding/json tag)
//   - flag:    flag name for CLI mode (e.g., --query "cats")
//   - desc:    help text shown with --help
//   - default: default value in CLI mode (JSON mode uses Go zero values)
//
// Supported field types: string, int, bool, float64.
//
// If stdin JSON is malformed, ParseArgs calls Error() and the skill exits.
// This is intentional — partial or corrupt input should never produce
// partial output.
func ParseArgs(dst any) {
	// Validate that dst is a pointer to a struct — anything else is a
	// programming error in the skill, so we bail loudly.
	rv := reflect.ValueOf(dst)
	if rv.Kind() != reflect.Ptr || rv.Elem().Kind() != reflect.Struct {
		Error("ParseArgs: dst must be a pointer to a struct")
		return
	}

	if isStdinPipe() {
		parseStdinJSON(dst)
		return
	}
	parseCLIFlags(dst)
}

// parseStdinJSON reads all of stdin and decodes it into dst.
// If stdin is empty (pipe exists but no data), we fall back to CLI flags.
func parseStdinJSON(dst any) {
	data, err := io.ReadAll(stdinReader)
	if err != nil {
		Error(fmt.Sprintf("failed to read stdin: %s", err))
		return
	}

	// Empty pipe — nothing was written. Fall back to flags.
	// This can happen if the harness opens a pipe but sends nothing.
	if len(data) == 0 {
		parseCLIFlags(dst)
		return
	}

	if err := json.Unmarshal(data, dst); err != nil {
		Error(fmt.Sprintf("invalid input JSON: %s", err))
	}
}

// parseCLIFlags uses reflection to build a flag set from struct tags,
// then parses os.Args.
//
// Reflection in Go is like Python's inspect module — it lets you examine
// types and values at runtime. We use it here to read the struct field
// tags (flag, desc, default) and wire them up to Go's flag package.
//
// Unlike Python where you can just setattr(obj, name, value), Go needs
// a pointer to the actual field in the struct. That's what
// v.Field(i).Addr().Interface() gives us — a pointer we can hand to
// flag.StringVar, flag.IntVar, etc.
func parseCLIFlags(dst any) {
	v := reflect.ValueOf(dst).Elem()
	t := v.Type()

	fs := flag.NewFlagSet("skill", flag.ContinueOnError)

	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		flagName := field.Tag.Get("flag")
		if flagName == "" {
			// No flag tag — skip this field. It's only populated via JSON.
			continue
		}

		desc := field.Tag.Get("desc")
		dflt := field.Tag.Get("default")

		// Get a pointer to the actual struct field so the flag package
		// can write directly into it. This is the Go equivalent of
		// getting a reference to an attribute in Python.
		ptr := v.Field(i).Addr().Interface()

		switch field.Type.Kind() {
		case reflect.String:
			fs.StringVar(ptr.(*string), flagName, dflt, desc)
		case reflect.Int:
			d, _ := strconv.Atoi(dflt) // zero on parse failure — fine as default
			fs.IntVar(ptr.(*int), flagName, d, desc)
		case reflect.Bool:
			d, _ := strconv.ParseBool(dflt)
			fs.BoolVar(ptr.(*bool), flagName, d, desc)
		case reflect.Float64:
			d, _ := strconv.ParseFloat(dflt, 64)
			fs.Float64Var(ptr.(*float64), flagName, d, desc)
		}
	}

	if err := fs.Parse(osArgs()); err != nil {
		Error(fmt.Sprintf("invalid flags: %s", err))
	}
}
