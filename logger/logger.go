// Package logger provides a structured logging bridge that routes log calls
// through the TUI event bus.
//
// Every package in the codebase does:
//
//	var log = logger.WithPrefix("agent")
//	log.Info("something happened", "key", "value")
//
// This bridge keeps that exact API but routes output to:
//  1. The event bus (for TUI rendering) — if initialized
//  2. A file logger (for debugging) — if initialized
//  3. Stderr via charmbracelet/log — as a fallback when neither is set
//
// Call logger.Init(bus, logWriter) early in main() before any log calls.
// If Init is never called (tests, simple CLI commands), everything falls
// back to stderr — backward compatible with the old behavior.
package logger

import (
	"fmt"
	"io"
	"os"
	"time"

	"her/tui"

	charmlog "github.com/charmbracelet/log"
)

// bus is the global event bus. Nil until Init() is called.
var bus *tui.Bus

// fileLogger writes to a log file for debugging when the TUI owns the terminal.
// Nil until Init() is called with a non-nil writer.
var fileLogger *charmlog.Logger

// fallback writes to stderr when no bus is configured (tests, pre-Init calls).
// This preserves the old behavior exactly.
var fallback *charmlog.Logger

func init() {
	fallback = charmlog.NewWithOptions(os.Stderr, charmlog.Options{
		ReportTimestamp: true,
		TimeFormat:      time.TimeOnly,
		Level:           charmlog.InfoLevel,
	})
}

// Init sets the global event bus and optional file logger. Must be called
// before any log calls if you want events routed to the TUI/file.
//
// Parameters:
//   - b: the event bus (required for TUI mode; nil to skip)
//   - logFile: an io.Writer for file logging (e.g. an *os.File; nil to skip)
func Init(b *tui.Bus, logFile io.Writer) {
	bus = b
	if logFile != nil {
		fileLogger = charmlog.NewWithOptions(logFile, charmlog.Options{
			ReportTimestamp: true,
			TimeFormat:      time.TimeOnly,
			Level:           charmlog.DebugLevel, // capture everything to file
		})
	}
}

// Logger is a prefixed logger that emits events to the bus. It has the same
// method set as *charmlog.Logger so all existing call sites compile unchanged.
//
// The methods match charmbracelet/log's signatures:
//   - Info(msg string, keyvals ...interface{})
//   - Infof(format string, args ...interface{})
//   - Warn, Error, Fatal, Debug — same pattern
type Logger struct {
	prefix string
}

// WithPrefix creates a sub-logger tagged with a component name.
// Same signature and usage as before — all 11 package-level declarations
// like `var log = logger.WithPrefix("agent")` work unchanged.
func WithPrefix(name string) *Logger {
	return &Logger{prefix: name}
}

// --- Structured methods (msg + key-value pairs) ---

// Info logs an informational message with optional structured key-value pairs.
func (l *Logger) Info(msg interface{}, keyvals ...interface{}) {
	l.emit(tui.LevelInfo, fmt.Sprint(msg), keyvals)
}

// Warn logs a warning message with optional structured key-value pairs.
func (l *Logger) Warn(msg interface{}, keyvals ...interface{}) {
	l.emit(tui.LevelWarn, fmt.Sprint(msg), keyvals)
}

// Error logs an error message with optional structured key-value pairs.
func (l *Logger) Error(msg interface{}, keyvals ...interface{}) {
	l.emit(tui.LevelError, fmt.Sprint(msg), keyvals)
}

// Debug logs a debug message with optional structured key-value pairs.
func (l *Logger) Debug(msg interface{}, keyvals ...interface{}) {
	l.emit(tui.LevelDebug, fmt.Sprint(msg), keyvals)
}

// Fatal logs a fatal message, writes to file, then exits.
// In TUI mode we still exit — Fatal means "can't continue."
func (l *Logger) Fatal(msg interface{}, keyvals ...interface{}) {
	l.emit(tui.LevelFatal, fmt.Sprint(msg), keyvals)
	os.Exit(1)
}

// --- Printf-style methods ---

// Infof logs a formatted informational message.
func (l *Logger) Infof(format string, args ...interface{}) {
	l.emit(tui.LevelInfo, fmt.Sprintf(format, args...), nil)
}

// Warnf logs a formatted warning message.
func (l *Logger) Warnf(format string, args ...interface{}) {
	l.emit(tui.LevelWarn, fmt.Sprintf(format, args...), nil)
}

// Errorf logs a formatted error message.
func (l *Logger) Errorf(format string, args ...interface{}) {
	l.emit(tui.LevelError, fmt.Sprintf(format, args...), nil)
}

// Debugf logs a formatted debug message.
func (l *Logger) Debugf(format string, args ...interface{}) {
	l.emit(tui.LevelDebug, fmt.Sprintf(format, args...), nil)
}

// emit is the internal workhorse. It routes to both the event bus and the
// file logger, falling back to stderr if neither is configured.
func (l *Logger) emit(level tui.Level, msg string, keyvals []interface{}) {
	fields := kvToMap(keyvals)

	// Route 1: event bus (for TUI)
	if bus != nil {
		bus.Emit(tui.LogEvent{
			Time:    time.Now(),
			Source:  l.prefix,
			Level:   level,
			Message: msg,
			Fields:  fields,
		})
	}

	// Route 2: file logger (for debugging)
	if fileLogger != nil {
		fl := fileLogger.WithPrefix(l.prefix)
		switch level {
		case tui.LevelDebug:
			fl.Debug(msg, keyvals...)
		case tui.LevelInfo:
			fl.Info(msg, keyvals...)
		case tui.LevelWarn:
			fl.Warn(msg, keyvals...)
		case tui.LevelError:
			fl.Error(msg, keyvals...)
		case tui.LevelFatal:
			fl.Fatal(msg, keyvals...)
		}
		return
	}

	// Route 3: stderr fallback (no bus, no file — tests, pre-Init)
	if bus == nil {
		fl := fallback.WithPrefix(l.prefix)
		switch level {
		case tui.LevelDebug:
			fl.Debug(msg, keyvals...)
		case tui.LevelInfo:
			fl.Info(msg, keyvals...)
		case tui.LevelWarn:
			fl.Warn(msg, keyvals...)
		case tui.LevelError:
			fl.Error(msg, keyvals...)
		case tui.LevelFatal:
			fl.Fatal(msg, keyvals...)
		}
	}
}

// kvToMap converts charmbracelet/log-style alternating key-value pairs
// into a map. E.g. ("err", someErr, "model", "deepseek") becomes
// {"err": someErr, "model": "deepseek"}.
//
// If there's an odd number of values, the last key gets a nil value.
func kvToMap(keyvals []interface{}) map[string]any {
	if len(keyvals) == 0 {
		return nil
	}
	m := make(map[string]any, len(keyvals)/2)
	for i := 0; i < len(keyvals); i += 2 {
		key := fmt.Sprint(keyvals[i])
		var val any
		if i+1 < len(keyvals) {
			val = keyvals[i+1]
		}
		m[key] = val
	}
	return m
}
