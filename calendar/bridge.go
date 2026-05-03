// Package calendar provides a Go wrapper around the Swift EventKit bridge.
// The Swift binary (her-calendar) handles the actual macOS Calendar integration
// via EventKit. This package shells out to it, handles retries, and provides
// a clean Go API for the calendar tools.
package calendar

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"her/config"
	"her/logger"
	"her/retry"
)

var log = logger.WithPrefix("calendar")

// Bridge is the interface for calendar operations. The production implementation
// (CLIBridge) shells out to the Swift EventKit binary. The test/sim implementation
// (FakeBridge) is in-memory. Both satisfy this contract so tools work unchanged.
type Bridge interface {
	Call(ctx context.Context, req Request) (Response, error)
}

// CLIBridge wraps the Swift her-calendar CLI with retry logic.
// Each method call spawns the Swift binary, pipes JSON in/out, and returns
// the result. No daemon, no persistent state — just clean request/response.
type CLIBridge struct {
	binaryPath string
	cfg        *config.Config
}

// NewCLIBridge creates a CLIBridge instance. Checks if the binary exists and is
// executable, but doesn't fail if it's missing (fail-soft pattern — tools
// will return clear errors later).
func NewCLIBridge(cfg *config.Config) *CLIBridge {
	// Resolve relative paths from project root
	binaryPath := cfg.Calendar.BridgePath
	if !filepath.IsAbs(binaryPath) {
		// Assume relative to the working directory (project root)
		absPath, err := filepath.Abs(binaryPath)
		if err != nil {
			log.Warn("Failed to resolve bridge path", "path", binaryPath, "error", err)
		} else {
			binaryPath = absPath
		}
	}

	// Check if binary exists
	if _, err := os.Stat(binaryPath); os.IsNotExist(err) {
		log.Warn("Calendar bridge not found — calendar tools will return errors if called",
			"path", binaryPath,
			"hint", "Build it with: cd calendar/bridge && swift build -c release")
	}

	return &CLIBridge{
		binaryPath: binaryPath,
		cfg:        cfg,
	}
}

// Request is the JSON structure sent to the Swift bridge via stdin.
// The "command" field determines which EventKit operation to perform.
type Request struct {
	Command  string         `json:"command"`  // "list", "create", "update", "delete"
	Calendar string         `json:"calendar"` // calendar name in Apple Calendar
	Args     map[string]any `json:"args"`     // command-specific arguments
}

// Response is the JSON structure returned by the Swift bridge via stdout.
// If OK is false, Error and Message contain the failure details.
type Response struct {
	OK      bool           `json:"ok"`
	Result  map[string]any `json:"result,omitempty"`  // present when OK=true
	Error   string         `json:"error,omitempty"`   // error code when OK=false
	Message string         `json:"message,omitempty"` // human-readable detail when OK=false
}

// Call executes a command via the Swift bridge with retry logic.
// errPermanent marks errors that should not be retried (exit code 2 =
// calendar-side errors like event not found).
type errPermanent struct{ err error }

func (e *errPermanent) Error() string { return e.err.Error() }
func (e *errPermanent) Unwrap() error { return e.err }

// Call executes a command via the Swift bridge with retry logic.
// Retries up to 3 times with exponential backoff on exit code 1
// (bridge errors like permission flakes). Exit code 2 (calendar-side errors like
// event not found) fails immediately.
func (b *CLIBridge) Call(ctx context.Context, req Request) (Response, error) {
	if _, err := os.Stat(b.binaryPath); os.IsNotExist(err) {
		return Response{}, fmt.Errorf("calendar bridge not found at %s — see calendar/bridge/README.md for build instructions", b.binaryPath)
	}

	var resp Response
	err := retry.Do(ctx, retry.Config{
		MaxAttempts: 3,
		Backoff:     retry.Exponential,
		InitialWait: 500 * time.Millisecond,
		IsRetriable: func(err error) bool {
			_, permanent := err.(*errPermanent)
			return !permanent
		},
	}, func() error {
		r, exitCode, err := b.callOnce(ctx, req)

		if err == nil && r.OK {
			resp = r
			return nil
		}

		// Exit code 2 = calendar-side error — not worth retrying.
		if exitCode == 2 {
			resp = r
			return &errPermanent{fmt.Errorf("calendar error: %s (%s)", r.Message, r.Error)}
		}

		if exitCode == 1 {
			return fmt.Errorf("bridge error (exit %d): %v", exitCode, err)
		}
		return err
	})

	if err != nil {
		// If the permanent error wrapper survived, unwrap it for the caller.
		if p, ok := err.(*errPermanent); ok {
			return resp, p.err
		}
		return Response{}, err
	}
	return resp, nil
}

// callOnce executes a single bridge call without retry logic.
// Returns the response, exit code, and any error that occurred.
func (b *CLIBridge) callOnce(ctx context.Context, req Request) (Response, int, error) {
	// Marshal request to JSON
	reqJSON, err := json.Marshal(req)
	if err != nil {
		return Response{}, 0, fmt.Errorf("failed to marshal request: %w", err)
	}

	// Create the command
	cmd := exec.CommandContext(ctx, b.binaryPath)
	cmd.Stdin = bytes.NewReader(reqJSON)

	// Capture stdout and stderr
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	// Run the command
	err = cmd.Run()

	// Check exit code
	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			// Process spawn failure or context cancellation
			return Response{}, 0, fmt.Errorf("failed to execute bridge: %w", err)
		}
	}

	// Parse response from stdout
	var resp Response
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		// If we can't parse the response, include stderr for debugging
		return Response{}, exitCode, fmt.Errorf("failed to parse bridge response (exit %d): %w\nstderr: %s", exitCode, err, stderr.String())
	}

	// Log stderr if present (even on success, for debugging)
	if stderr.Len() > 0 {
		log.Debug("Bridge stderr output", "stderr", stderr.String())
	}

	return resp, exitCode, nil
}
