// Package logger provides a pre-configured charmbracelet/log logger
// for the entire application.
//
// The problem this solves: if you use charmlog.With() at package init
// time (like `var log = charmlog.With("component", "bot")`), it copies
// the default logger's settings at that moment — before main() runs.
// Any configuration done later in runBot() won't propagate to those
// sub-loggers.
//
// By creating the base logger here with all settings baked in, every
// sub-logger created via logger.With() inherits the right config
// regardless of when it's created.
//
// This is similar to how Python's logging.getLogger("name") works —
// there's a root logger with shared configuration, and child loggers
// inherit from it. In Go, we do it explicitly with a package-level var.
package logger

import (
	"os"
	"time"

	charmlog "github.com/charmbracelet/log"
)

// Base is the root logger for the application. All package-level
// loggers should be created from this via With().
//
// Configured with:
//   - Timestamps in HH:MM:SS format
//   - Info level by default
//   - Auto-detects TTY for colors (pretty in terminal, plain in log files)
var Base *charmlog.Logger

func init() {
	Base = charmlog.NewWithOptions(os.Stderr, charmlog.Options{
		ReportTimestamp: true,
		TimeFormat:      time.TimeOnly,
		Level:           charmlog.InfoLevel,
	})
}

// With creates a sub-logger with additional structured key-value context.
func With(keyvals ...interface{}) *charmlog.Logger {
	return Base.With(keyvals...)
}

// WithPrefix creates a sub-logger with a bracket-wrapped prefix,
// e.g. logger.WithPrefix("agent") shows "[agent]" before each message.
// This is the preferred way to tag log lines by component —
// it reads like the old [agent] / [bot] style but with charm's formatting.
func WithPrefix(name string) *charmlog.Logger {
	return Base.WithPrefix(name)
}
