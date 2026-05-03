package tui

import (
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"her/config"
)

// View renders the entire TUI. Called after every Update().
//
// This is like React's render() — it reads state and produces output.
// Never mutate state here. The returned string IS the entire terminal
// contents (Bubble Tea handles the diff and repaint).
// View returns a tea.View — Bubble Tea v2's way of returning rendered content.
// In v1, View() returned a plain string. In v2, it returns a tea.View struct
// that wraps the content string. This lets the view also control alt screen
// mode, mouse mode, and window title per-frame.
func (m Model) View() tea.View {
	if m.width == 0 || m.height == 0 {
		return tea.View{Content: "Initializing..."}
	}

	var b strings.Builder

	// --- Header (2 lines) ---
	b.WriteString(m.renderHeader())
	b.WriteString("\n")

	// --- Body (scrollable sections) ---
	bodyLines := m.renderBody()
	bodyH := m.bodyHeight()

	// Apply scroll offset — slice the visible window
	scroll := m.scroll
	if scroll > len(bodyLines) {
		scroll = len(bodyLines)
	}
	end := scroll + bodyH
	if end > len(bodyLines) {
		end = len(bodyLines)
	}
	visible := bodyLines[scroll:end]

	// Pad to fill the body area (prevents footer from jumping around)
	for len(visible) < bodyH {
		visible = append(visible, "")
	}

	for _, line := range visible {
		// Truncate long lines to terminal width
		if lipgloss.Width(line) > m.width {
			line = line[:m.width]
		}
		b.WriteString(line)
		b.WriteString("\n")
	}

	// --- Footer (1 line) ---
	b.WriteString(m.renderFooter())

	return tea.View{
		Content:   b.String(),
		AltScreen: true,                    // full terminal takeover
		MouseMode: tea.MouseModeCellMotion, // enable mouse clicks + scroll wheel
	}
}

// renderHeader builds the top status bar.
func (m Model) renderHeader() string {
	title := headerTitleStyle.Render(" her ")

	uptime := time.Since(m.startTime).Round(time.Second)
	uptimeStr := headerStatStyle.Render("uptime: ") +
		headerStatValueStyle.Render(formatDuration(uptime))

	msgsStr := headerStatStyle.Render("msgs: ") +
		headerStatValueStyle.Render(fmt.Sprintf("%d", m.messageCount))

	costStr := headerStatStyle.Render("cost: ") +
		headerStatValueStyle.Render(fmt.Sprintf("$%.4f", m.totalCost))

	sep := headerStatStyle.Render(" · ")
	stats := uptimeStr + sep + msgsStr + sep + costStr

	// Fill the header to full width
	content := title + "  " + stats
	padding := m.width - lipgloss.Width(content)
	if padding > 0 {
		content += strings.Repeat(" ", padding)
	}

	return headerStyle.Width(m.width).Render(content)
}

// renderBody produces all the lines for sections (both collapsed and expanded).
func (m Model) renderBody() []string {
	var lines []string

	for i := range m.sections {
		sec := &m.sections[i]

		// Section header line (always visible)
		header := m.renderSectionHeader(i, sec)
		lines = append(lines, header)

		// Expanded content
		if sec.Expanded {
			if sec.Kind == "turn" {
				lines = append(lines, m.renderTurnContent(sec)...)
			} else {
				for _, line := range sec.Lines {
					lines = append(lines, "  "+line)
				}
			}
		}
	}

	return lines
}

// renderTurnContent renders the grouped content within an expanded turn section.
// Each non-empty group gets a bordered box with a label.
func (m Model) renderTurnContent(sec *Section) []string {
	var lines []string

	// Available width for content boxes (account for 4-char indent)
	boxWidth := m.width - 6
	if boxWidth < 40 {
		boxWidth = 40
	}

	type group struct {
		label string
		emoji string
		lines []string
		style lipgloss.Style
	}

	groups := []group{
		{"context", "📋", sec.ContextLines, lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorDim).Width(boxWidth).Padding(0, 1)},
		{"driver", "🛠️", sec.ToolLines, lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorCyan).Width(boxWidth).Padding(0, 1)},
		{"reply", "💬", sec.ReplyLines, lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorSuccess).Width(boxWidth).Padding(0, 1)},
		{"persona", "🪞", sec.PersonaLines, lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorPurple).Width(boxWidth).Padding(0, 1)},
		{"memory", "🧩", sec.MemoryLines, lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorDim).Width(boxWidth).Padding(0, 1)},
		{"mood", "🎭", sec.MoodLines, lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(colorPurple).Width(boxWidth).Padding(0, 1)},
	}

	for _, g := range groups {
		if len(g.lines) == 0 {
			continue
		}

		// Build content with a header line showing the agent's emoji + label
		header := lipgloss.NewStyle().Bold(true).Render(g.emoji + " " + g.label)
		content := header + "\n" + strings.Join(g.lines, "\n")

		// Render the bordered box
		box := g.style.Render(content)

		// Split the box into lines and indent each
		for _, boxLine := range strings.Split(box, "\n") {
			lines = append(lines, "   "+boxLine)
		}
	}

	return lines
}

// renderSectionHeader renders the one-line summary for a section.
func (m Model) renderSectionHeader(idx int, sec *Section) string {
	// Cursor indicator
	cursor := "  "
	if idx == m.cursor {
		cursor = sectionCursorStyle.Render("▸ ")
	}

	// Expand/collapse indicator
	indicator := "▶"
	if sec.Expanded {
		indicator = "▼"
	}

	// Timestamp
	ts := sectionTimestampStyle.Render(sec.Timestamp.Format("15:04:05"))

	// Build the rest based on section kind
	var detail string
	switch sec.Kind {
	case "turn":
		detail = m.renderTurnHeader(sec)
	case "startup":
		detail = headerTitleStyle.Render("Startup")
		if !sec.Expanded {
			detail += sectionMetricsStyle.Render(
				fmt.Sprintf(" (%d events)", len(sec.Lines)),
			)
		}
	case "sidecar":
		detail = logInfoStyle.Render(strings.ToUpper(sec.ID))
		if !sec.Expanded {
			detail += sectionMetricsStyle.Render(
				fmt.Sprintf(" (%d lines)", len(sec.Lines)),
			)
		}
	default:
		detail = logInfoStyle.Render(sec.Title)
	}

	// Highlight the current cursor row
	line := fmt.Sprintf("%s%s %s  %s", cursor, indicator, ts, detail)
	if idx == m.cursor {
		// Subtle highlight on the cursor row
		line = lipgloss.NewStyle().
			Background(lipgloss.Color("236")).
			Width(m.width).
			Render(line)
	}

	return line
}

// renderTurnHeader builds the detail portion of a turn section header.
func (m Model) renderTurnHeader(sec *Section) string {
	// Title in blue
	title := lipgloss.NewStyle().
		Foreground(colorTurn).
		Bold(true).
		Render(sec.Title)

	// User message preview (truncated)
	msgPreview := ""
	if sec.UserMessage != "" {
		preview := sec.UserMessage
		if len(preview) > 40 {
			preview = preview[:40] + "…"
		}
		msgPreview = lipgloss.NewStyle().
			Foreground(colorSubtle).
			Render(fmt.Sprintf(" %q", preview))
	}

	// Metrics (only show if we have data — TurnEndEvent updates these)
	metrics := ""
	if sec.CostUSD > 0 || sec.LatencyMs > 0 || sec.ToolCount > 0 {
		parts := []string{}
		if sec.CostUSD > 0 {
			parts = append(parts, fmt.Sprintf("$%.4f", sec.CostUSD))
		}
		if sec.LatencyMs > 0 {
			parts = append(parts, fmt.Sprintf("%.1fs", float64(sec.LatencyMs)/1000))
		}
		if sec.ToolCount > 0 {
			parts = append(parts, fmt.Sprintf("%d tools", sec.ToolCount))
		}
		metrics = sectionMetricsStyle.Render("  " + strings.Join(parts, " · "))
	}

	return title + msgPreview + metrics
}

// renderFooter builds the bottom key binding help bar.
func (m Model) renderFooter() string {
	bindings := []struct{ key, desc string }{
		{"click", "toggle"},
		{"scroll", "navigate"},
		{"e/c", "expand/collapse all"},
		{"y", "copy ID"},
		{"q", "quit"},
	}

	var parts []string
	for _, b := range bindings {
		parts = append(parts,
			footerKeyStyle.Render(b.key)+" "+footerDescStyle.Render(b.desc),
		)
	}

	// Scroll indicator on the right
	totalLines := m.countRenderedLines()
	bodyH := m.bodyHeight()
	scrollInfo := ""
	if totalLines > bodyH {
		pct := float64(m.scroll) / float64(totalLines-bodyH) * 100
		scrollInfo = footerDescStyle.Render(fmt.Sprintf("  %.0f%%", pct))
	}

	return footerStyle.Render(strings.Join(parts, "  ") + scrollInfo)
}

// --- Event renderers ---
// Each event type has a renderer that produces a styled string line.

func renderLogEvent(ev LogEvent) string {
	var style lipgloss.Style
	switch ev.Level {
	case LevelDebug:
		style = logDebugStyle
	case LevelInfo:
		style = logInfoStyle
	case LevelWarn:
		style = logWarnStyle
	case LevelError:
		style = logErrorStyle
	case LevelFatal:
		style = logFatalStyle
	}

	line := fmt.Sprintf("%s %s", ev.Level, ev.Message)
	if len(ev.Fields) > 0 {
		var pairs []string
		for k, v := range ev.Fields {
			pairs = append(pairs, fmt.Sprintf("%s=%v", k, v))
		}
		line += " " + strings.Join(pairs, " ")
	}
	return style.Render(line)
}

func renderStartupEvent(ev StartupEvent) string {
	// Status icon
	icon := "⏳"
	style := logInfoStyle
	switch ev.Status {
	case "ready":
		icon = "✓"
		style = lipgloss.NewStyle().Foreground(colorSuccess)
	case "skipped":
		icon = "○"
		style = logDebugStyle
	case "failed":
		icon = "✗"
		style = logErrorStyle
	}

	line := fmt.Sprintf("%s %s", icon, ev.Phase)
	if ev.Detail != "" {
		line += logDebugStyle.Render("  " + ev.Detail)
	}
	return style.Render(line)
}

func renderContextEvent(ev ContextEvent) string {
	return subHeaderStyle.Render("context ready (recall-driven)")
}

func renderAgentIterEvent(ev AgentIterEvent) string {
	return logInfoStyle.Render(
		fmt.Sprintf("tokens: %d+%d",
			ev.PromptTokens, ev.CompletionTokens)) +
		sectionMetricsStyle.Render(
			fmt.Sprintf(" | $%.6f | %s",
				ev.CostUSD, ev.FinishReason))
}

func renderToolCallEvent(ev ToolCallEvent) string {
	// Handle errors first — override normal rendering with error styling
	if ev.IsError {
		name := toolNameStyle.Render(ev.ToolName)
		result := ev.Result
		if len(result) > 80 {
			result = result[:80] + "…"
		}
		return fmt.Sprintf("❌ %s %s %s",
			name,
			toolArrowStyle.Render("→"),
			logErrorStyle.Render(result))
	}

	// Different icons for different tool types
	icon := "🔧"
	switch ev.ToolName {
	case "think":
		icon = "🧠"
		// For think, extract just the thought text from the JSON args.
		// Args look like: {"thought":"User is feeling restless..."}
		thought := ev.Args
		// Try to extract the thought value from JSON
		if idx := strings.Index(thought, `"thought":"`); idx >= 0 {
			start := idx + len(`"thought":"`)
			if end := strings.LastIndex(thought[start:], `"`); end >= 0 {
				thought = thought[start : start+end]
			}
		}
		if len(thought) > 120 {
			thought = thought[:120] + "…"
		}
		return thinkStyle.Render(fmt.Sprintf("%s %s", icon, thought))
	case "reply":
		icon = "📝"
	case "save_memory", "update_memory":
		icon = "💾"
	case "remove_memory":
		icon = "🗑"
	case "web_search":
		icon = "🔍"
	case "web_read":
		icon = "🌐"
	case "book_search":
		icon = "📚"
	case "view_image":
		icon = "👁"
	case "done":
		icon = "✅"
	case "fact→chat":
		// Special rendering for injected facts — show the ID/source
		// from Args alongside a truncated fact preview from Result.
		icon = "📎"
		meta := lipgloss.NewStyle().Foreground(colorCost).Render(ev.Args)
		fact := ev.Result
		if len(fact) > 60 {
			fact = fact[:60] + "…"
		}
		return fmt.Sprintf("%s %s %s", icon, meta, toolResultStyle.Render(fact))
	case "no_action":
		icon = "⏭"
	case "use_tools":
		icon = "🧰"
	case "log_mood":
		icon = "💭"
	case "create_reminder", "create_schedule":
		icon = "⏰"
	}

	name := toolNameStyle.Render(ev.ToolName)
	result := ev.Result
	if len(result) > 80 {
		result = result[:80] + "…"
	}
	return fmt.Sprintf("%s %s %s %s",
		icon,
		name,
		toolArrowStyle.Render("→"),
		toolResultStyle.Render(result))
}

func renderReplyEvent(ev ReplyEvent, cfg *config.Config) string {
	metrics := replyMetricsStyle.Render(
		fmt.Sprintf("%d+%d=%d tokens | $%.6f | %dms",
			ev.PromptTokens, ev.CompletionTokens, ev.TotalTokens,
			ev.CostUSD, ev.LatencyMs))

	text := ev.Text
	if len(text) > 100 {
		text = text[:100] + "…"
	}
	reply := replyTextStyle.Render(strings.ToLower(cfg.Identity.Her) + ": " + text)

	return metrics + "\n" + reply
}

func renderPersonaEvent(ev PersonaEvent) string {
	icon := "🪞"
	switch ev.Action {
	case "reflection_triggered":
		icon = "💭"
	case "rewrite_triggered":
		icon = "✨"
	}
	return personaStyle.Render(fmt.Sprintf("%s %s %s", icon, ev.Action, ev.Detail))
}

func renderMoodEvent(ev MoodEvent) string {
	icon := "🎭"
	switch {
	case strings.HasPrefix(ev.Action, "auto_logged"):
		icon = "✅"
	case strings.HasPrefix(ev.Action, "updated"):
		icon = "♻️"
	case strings.HasPrefix(ev.Action, "proposal"):
		icon = "📩"
	case strings.HasPrefix(ev.Action, "dropped"):
		icon = "⏭"
	case strings.HasPrefix(ev.Action, "errored"):
		icon = "⚠️"
	}

	detail := ev.Action
	if ev.Valence > 0 {
		labels := strings.Join(ev.Labels, ", ")
		detail = fmt.Sprintf("%s v=%d [%s] conf=%.2f", ev.Action, ev.Valence, labels, ev.Confidence)
	}
	if ev.Reason != "" && strings.HasPrefix(ev.Action, "dropped") {
		detail += " — " + ev.Reason
	}
	return logInfoStyle.Render(fmt.Sprintf("%s %s", icon, detail))
}

func renderSidecarEvent(ev SidecarEvent) string {
	style := logDebugStyle
	if ev.IsErr {
		style = logWarnStyle
	}
	return style.Render(ev.Line)
}

// --- Helpers ---

// formatDuration formats a duration as "1h23m" or "45s" etc.
func formatDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%dm", int(d.Hours()), int(d.Minutes())%60)
}
