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
	"github.com/charmbracelet/log"
)

// Bridge wraps the Swift her-calendar CLI with retry logic.
// Each method call spawns the Swift binary, pipes JSON in/out, and returns
// the result. No daemon, no persistent state — just clean request/response.
type Bridge struct {
	binaryPath string
	cfg        *config.Config
	logger     *log.Logger
}

// NewBridge creates a Bridge instance. Checks if the binary exists and is
// executable, but doesn't fail if it's missing (fail-soft pattern — tools
// will return clear errors later).
func NewBridge(cfg *config.Config, logger *log.Logger) *Bridge {
	// Resolve relative paths from project root
	binaryPath := cfg.Calendar.BridgePath
	if !filepath.IsAbs(binaryPath) {
		// Assume relative to the working directory (project root)
		absPath, err := filepath.Abs(binaryPath)
		if err != nil {
			logger.Warn("Failed to resolve bridge path", "path", binaryPath, "error", err)
		} else {
			binaryPath = absPath
		}
	}

	// Check if binary exists
	if _, err := os.Stat(binaryPath); os.IsNotExist(err) {
		logger.Warn("Calendar bridge not found — calendar tools will return errors if called",
			"path", binaryPath,
			"hint", "Build it with: cd calendar/bridge && swift build -c release")
	}

	return &Bridge{
		binaryPath: binaryPath,
		cfg:        cfg,
		logger:     logger,
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
// Retries up to 3 times with exponential backoff (500ms, 1s, 2s) on exit code 1
// (bridge errors like permission flakes). Exit code 2 (calendar-side errors like
// event not found) fails immediately. Returns an error if all retries fail or if
// the bridge binary is missing.
func (b *Bridge) Call(ctx context.Context, req Request) (Response, error) {
	// Check if binary exists
	if _, err := os.Stat(b.binaryPath); os.IsNotExist(err) {
		return Response{}, fmt.Errorf("calendar bridge not found at %s — see calendar/bridge/README.md for build instructions", b.binaryPath)
	}

	// Retry configuration: 3 attempts with exponential backoff
	maxAttempts := 3
	backoffDurations := []time.Duration{0, 500 * time.Millisecond, 1 * time.Second, 2 * time.Second}

	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		// Sleep before retry (not on first attempt)
		if attempt > 1 {
			backoff := backoffDurations[attempt-1]
			b.logger.Debug("Retrying bridge call", "attempt", attempt, "backoff", backoff)
			time.Sleep(backoff)
		}

		// Execute the call
		resp, exitCode, err := b.callOnce(ctx, req)

		// Success path
		if err == nil && resp.OK {
			return resp, nil
		}

		// Exit code 2 = calendar-side error (event not found, etc.) — fail immediately
		if exitCode == 2 {
			return resp, fmt.Errorf("calendar error: %s (%s)", resp.Message, resp.Error)
		}

		// Exit code 1 = bridge error (permission flake, EventKit locked, bad JSON) — retry
		if exitCode == 1 {
			lastErr = fmt.Errorf("bridge error (exit %d): %v", exitCode, err)
			continue
		}

		// Other errors (process spawn failure, etc.) — retry
		lastErr = err
	}

	// All retries exhausted
	return Response{}, fmt.Errorf("bridge call failed after %d attempts: %w", maxAttempts, lastErr)
}

// callOnce executes a single bridge call without retry logic.
// Returns the response, exit code, and any error that occurred.
func (b *Bridge) callOnce(ctx context.Context, req Request) (Response, int, error) {
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
		b.logger.Debug("Bridge stderr output", "stderr", stderr.String())
	}

	return resp, exitCode, nil
}
