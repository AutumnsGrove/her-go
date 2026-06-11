// Package narrate_report implements the narrate_report tool — reads a report
// file from the reports directory and queues it for narration as a voice memo.
//
// The report text is sanitized for natural speech: markdown formatting,
// URLs, code blocks, and structural noise are stripped so Piper reads
// it like a person would narrate it.
//
// The actual TTS happens after the turn completes — the cleaned text is
// stored on tools.Context.PendingNarration and the bot layer sends the
// voice memo after the reply's own TTS finishes.
package narrate_report

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"her/logger"
	"her/tools"
)

var log = logger.WithPrefix("tools/narrate_report")

func init() {
	tools.Register("narrate_report", Handle)
}

// Handle reads a report, sanitizes it for speech, and queues it for
// narration after the turn completes.
func Handle(argsJSON string, ctx *tools.Context) string {
	var args struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return "error: " + err.Error()
	}
	if args.Path == "" {
		return "error: path is required"
	}

	if ctx.TTSCallback == nil {
		return "error: voice is not available right now"
	}

	absPath, err := tools.ValidateReportPath(ctx.ReportsDir, args.Path)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}

	content, err := os.ReadFile(absPath)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Sprintf("error: report not found: %s", args.Path)
		}
		return fmt.Sprintf("error reading report: %v", err)
	}

	cleaned := SanitizeForSpeech(string(content))
	if cleaned == "" {
		return "error: report is empty after sanitization"
	}

	// Queue the narration — the bot layer sends it after the reply TTS.
	ctx.PendingNarration = cleaned

	log.Infof("  narrate_report: %s (%d chars → %d chars speech)", args.Path, len(content), len(cleaned))
	return fmt.Sprintf("narration queued for %s (%d characters) — voice memo will be sent after your reply", args.Path, len(cleaned))
}

// SanitizeForSpeech strips markdown formatting and structural elements
// so the text reads naturally when spoken aloud by TTS. Exported so
// tests can call it directly.
func SanitizeForSpeech(md string) string {
	lines := strings.Split(md, "\n")
	var out []string

	inCodeBlock := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)

		// Skip code blocks entirely.
		if strings.HasPrefix(trimmed, "```") {
			inCodeBlock = !inCodeBlock
			continue
		}
		if inCodeBlock {
			continue
		}

		// Skip horizontal rules.
		if trimmed == "---" || trimmed == "***" || trimmed == "___" {
			continue
		}

		// Skip empty lines.
		if trimmed == "" {
			continue
		}

		// Strip heading markers — keep the text, add period for pause.
		if strings.HasPrefix(trimmed, "#") {
			trimmed = strings.TrimLeft(trimmed, "# ")
			if !strings.HasSuffix(trimmed, ".") && !strings.HasSuffix(trimmed, "?") && !strings.HasSuffix(trimmed, "!") {
				trimmed += "."
			}
		}

		// Strip bullet markers.
		trimmed = stripBullet(trimmed)

		// Apply inline cleanup.
		trimmed = cleanInline(trimmed)

		if trimmed != "" {
			out = append(out, trimmed)
		}
	}

	return strings.Join(out, "\n")
}

var (
	reLinkFull = regexp.MustCompile(`\[([^\]]+)\]\([^)]+\)`)
	reURL      = regexp.MustCompile(`https?://\S+`)
	reBold     = regexp.MustCompile(`\*\*([^*]+)\*\*|__([^_]+)__`)
	reItalic   = regexp.MustCompile(`\*([^*]+)\*|_([^_]+)_`)
	reCode     = regexp.MustCompile("`([^`]+)`")
	reImage    = regexp.MustCompile(`!\[([^\]]*)\]\([^)]+\)`)
)

func cleanInline(s string) string {
	s = reImage.ReplaceAllString(s, "$1")
	s = reLinkFull.ReplaceAllString(s, "$1")
	s = reBold.ReplaceAllStringFunc(s, func(m string) string {
		m = strings.TrimPrefix(m, "**")
		m = strings.TrimSuffix(m, "**")
		m = strings.TrimPrefix(m, "__")
		m = strings.TrimSuffix(m, "__")
		return m
	})
	s = reItalic.ReplaceAllStringFunc(s, func(m string) string {
		m = strings.Trim(m, "*")
		m = strings.Trim(m, "_")
		return m
	})
	s = reCode.ReplaceAllString(s, "$1")
	s = reURL.ReplaceAllString(s, "")
	s = strings.Join(strings.Fields(s), " ")
	return s
}

func stripBullet(s string) string {
	if strings.HasPrefix(s, "- ") {
		return s[2:]
	}
	if strings.HasPrefix(s, "* ") {
		return s[2:]
	}
	for i := 0; i < len(s); i++ {
		if s[i] >= '0' && s[i] <= '9' {
			continue
		}
		if s[i] == '.' && i > 0 && i+1 < len(s) && s[i+1] == ' ' {
			return s[i+2:]
		}
		break
	}
	return s
}
