package tui

import (
	"fmt"
	"io"
	"time"

	charmlog "github.com/charmbracelet/log"
)

// StartFileLogger subscribes to the bus and writes all events to a log file
// in a format similar to the old charmbracelet/log stderr output. This ensures
// you can still debug with `tail -f her.log` while the TUI owns the terminal.
//
// Runs in its own goroutine — returns immediately. The goroutine exits when
// the bus is closed (channel range loop ends naturally).
func StartFileLogger(bus *Bus, w io.Writer) {
	ch := bus.Subscribe(512) // larger buffer since disk I/O can lag

	logger := charmlog.NewWithOptions(w, charmlog.Options{
		ReportTimestamp: true,
		TimeFormat:      time.TimeOnly,
		Level:           charmlog.DebugLevel, // capture everything
	})

	go func() {
		for event := range ch {
			switch e := event.(type) {

			case LogEvent:
				// Reconstruct charmbracelet/log output from the LogEvent fields
				fl := logger.WithPrefix(e.Source)
				kvs := mapToKVSlice(e.Fields)
				switch e.Level {
				case LevelDebug:
					fl.Debug(e.Message, kvs...)
				case LevelInfo:
					fl.Info(e.Message, kvs...)
				case LevelWarn:
					fl.Warn(e.Message, kvs...)
				case LevelError:
					fl.Error(e.Message, kvs...)
				case LevelFatal:
					fl.Error("[FATAL] "+e.Message, kvs...) // don't call Fatal — would exit
				}

			case StartupEvent:
				logger.WithPrefix("startup").Info(
					fmt.Sprintf("[%s] %s", e.Phase, e.Status),
					"detail", e.Detail,
				)

			case TurnStartEvent:
				logger.WithPrefix("bot").Info(
					"─── incoming message ───",
					"turn_id", e.TurnID,
					"msg", e.UserMessage,
				)

			case AgentIterEvent:
				logger.WithPrefix("driver").Info(
					fmt.Sprintf("tokens: %d prompt + %d completion | $%.6f | finish=%s",
						e.PromptTokens, e.CompletionTokens, e.CostUSD, e.FinishReason),
					"turn_id", e.TurnID,
					"iter", e.Iteration,
				)

			case ToolCallEvent:
				prefix := "→"
				if e.IsError {
					prefix = "✗"
				}
				logger.WithPrefix("driver").Info(
					fmt.Sprintf("%s %s: %s", prefix, e.ToolName, e.Result),
					"turn_id", e.TurnID,
					"is_error", e.IsError,
				)

			case ContextEvent:
				logger.WithPrefix("driver").Info(
					"context ready (recall-driven)",
					"turn_id", e.TurnID,
				)

			case ReplyEvent:
				logger.WithPrefix("driver").Info(
					fmt.Sprintf("reply: %d+%d=%d | $%.6f | %dms",
						e.PromptTokens, e.CompletionTokens, e.TotalTokens, e.CostUSD, e.LatencyMs),
					"turn_id", e.TurnID,
					"text", truncate(e.Text, 100),
				)

			case TurnEndEvent:
				logger.WithPrefix("bot").Info(
					"─── reply sent ───",
					"turn_id", e.TurnID,
					"cost", fmt.Sprintf("$%.6f", e.TotalCost),
					"elapsed_ms", e.ElapsedMs,
					"tools", e.ToolCalls,
				)

			case PersonaEvent:
				logger.WithPrefix("persona").Info(e.Action, "detail", e.Detail)

			case SidecarEvent:
				prefix := e.Sidecar
				if e.IsErr {
					logger.WithPrefix(prefix).Warn(e.Line)
				} else {
					logger.WithPrefix(prefix).Info(e.Line)
				}

			case CompactStartEvent:
				logger.WithPrefix("compact").Info("compacting", "stream", e.Stream)

			case CompactEvent:
				logger.WithPrefix("compact").Info("compacted",
					"msgs", e.Summarized,
					"before", e.TokensBefore,
					"after", e.TokensAfter,
					"saved", e.TokensBefore-e.TokensAfter)
			}
		}
	}()
}

// mapToKVSlice converts a map back to alternating key-value pairs for
// charmbracelet/log's variadic methods. E.g. {"err": "timeout"} becomes
// ["err", "timeout"].
func mapToKVSlice(m map[string]any) []interface{} {
	if m == nil {
		return nil
	}
	kvs := make([]interface{}, 0, len(m)*2)
	for k, v := range m {
		kvs = append(kvs, k, v)
	}
	return kvs
}

// truncate shortens a string to maxLen characters, appending "..." if truncated.
func truncate(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "..."
}
