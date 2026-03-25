package tui

import (
	"fmt"
	"time"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
)

// Section is one collapsible block in the TUI. Could be:
//   - "startup" — initialization logs
//   - "turn"    — a message turn (agent loop + reply)
//   - "sidecar" — STT/TTS process output
type Section struct {
	ID        string // unique: "startup", "turn-42", "stt", "tts"
	Title     string // display title in the header row
	Kind      string // "startup", "turn", "sidecar"
	Expanded  bool
	Lines     []string // pre-rendered content lines (for non-turn sections)
	Timestamp time.Time

	// Turn-specific: structured content groups instead of flat lines.
	// Each group gets its own bordered box when rendered.
	ContextLines []string // fact counts, semantic search results
	ToolLines    []string // agent iterations + tool calls
	ReplyLines   []string // reply metrics + mira's response
	PersonaLines []string // reflection/rewrite events

	// Turn-specific metadata for the collapsed one-line summary
	UserMessage string
	CostUSD     float64
	LatencyMs   int64
	ToolCount   int
	TurnID      int64
}

// Model is the Bubble Tea model for the entire TUI.
//
// In Bubble Tea, the Model is your single source of truth — like a
// Redux store. View() reads it to render, Update() writes to it when
// events arrive. Never render directly; always update state and let
// View() do the work.
type Model struct {
	sections []Section
	cursor   int // which section the cursor is on
	scroll   int // vertical scroll offset (in rendered lines)
	width    int
	height   int
	keys     KeyMap

	// Header stats
	startTime    time.Time
	messageCount int
	totalCost    float64

	// Event channel bridge from Bus → Bubble Tea
	eventCh <-chan Event

	// Quit signal to send back to cmd/run.go
	quitting bool
	quitCh   chan<- struct{}
}

// NewModel creates a TUI model connected to the event bus.
// quitCh is signaled when the user presses q — cmd/run.go uses this
// to trigger graceful shutdown of the bot and sidecars.
func NewModel(eventCh <-chan Event, quitCh chan<- struct{}) Model {
	return Model{
		keys:      DefaultKeyMap(),
		eventCh:   eventCh,
		startTime: time.Now(),
		quitCh:    quitCh,
	}
}

// eventMsg wraps an Event so it flows through Bubble Tea's message system.
// Bubble Tea's Update() receives tea.Msg values — this bridges our Event
// interface into that world.
type eventMsg struct{ event Event }

// listenForEvents returns a tea.Cmd that blocks on the event channel and
// delivers the next event as a tea.Msg. This is the standard Bubble Tea
// pattern for receiving data from external goroutines.
//
// Think of tea.Cmd as a Promise/Future — Bubble Tea runs it in a goroutine
// and delivers the result to Update(). We chain these: each event delivery
// triggers a new listen, creating an infinite "wait → deliver → wait" loop.
func (m Model) listenForEvents() tea.Cmd {
	return func() tea.Msg {
		event, ok := <-m.eventCh
		if !ok {
			return tea.Quit // channel closed → shutdown
		}
		return eventMsg{event: event}
	}
}

// Init is called once when the program starts. It kicks off the event
// listener and returns the initial model.
func (m Model) Init() tea.Cmd {
	return m.listenForEvents()
}

// Update handles all incoming messages — keyboard input, mouse clicks,
// window resizes, and our custom eventMsg from the bus.
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {

	case tea.WindowSizeMsg:
		m.width = msg.Width
		m.height = msg.Height
		return m, nil

	case tea.KeyPressMsg:
		return m.handleKey(msg)

	case tea.MouseClickMsg:
		return m.handleMouseClick(msg)

	case tea.MouseWheelMsg:
		return m.handleMouseWheel(msg)

	case eventMsg:
		m.handleEvent(msg.event)
		return m, m.listenForEvents() // keep listening
	}

	return m, nil
}

// handleMouseClick determines which section was clicked based on the
// y-coordinate and toggles it. This maps screen coordinates back to
// our section list, accounting for scroll offset and the header.
func (m Model) handleMouseClick(msg tea.MouseClickMsg) (tea.Model, tea.Cmd) {
	mouse := tea.Mouse(msg)

	// Y=0 and Y=1 are the header — ignore clicks there
	// The body starts at Y=2
	bodyY := mouse.Y - 2
	if bodyY < 0 {
		return m, nil
	}

	// Map the click Y to a section, accounting for scroll
	targetLine := m.scroll + bodyY
	lineCount := 0
	for i := range m.sections {
		sectionStart := lineCount
		sectionH := m.sectionHeight(i)
		lineCount += sectionH

		if targetLine >= sectionStart && targetLine < lineCount {
			// Click landed in section i
			if targetLine == sectionStart {
				// Clicked on the header line — toggle expand/collapse
				m.cursor = i
				m.sections[i].Expanded = !m.sections[i].Expanded
			} else {
				// Clicked on content — just move cursor there
				m.cursor = i
			}
			return m, nil
		}
	}

	return m, nil
}

// handleMouseWheel scrolls the viewport up or down.
func (m Model) handleMouseWheel(msg tea.MouseWheelMsg) (tea.Model, tea.Cmd) {
	mouse := tea.Mouse(msg)
	switch mouse.Button {
	case tea.MouseWheelUp:
		m.scroll -= 3
		if m.scroll < 0 {
			m.scroll = 0
		}
	case tea.MouseWheelDown:
		m.scroll += 3
		maxScroll := m.countRenderedLines() - m.bodyHeight()
		if maxScroll < 0 {
			maxScroll = 0
		}
		if m.scroll > maxScroll {
			m.scroll = maxScroll
		}
	}
	return m, nil
}

// handleKey processes keyboard input.
func (m Model) handleKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch {
	case key.Matches(msg, m.keys.Quit):
		m.quitting = true
		if m.quitCh != nil {
			close(m.quitCh)
		}
		return m, tea.Quit

	case key.Matches(msg, m.keys.Up):
		if m.cursor > 0 {
			m.cursor--
			m.ensureCursorVisible()
		}

	case key.Matches(msg, m.keys.Down):
		if m.cursor < len(m.sections)-1 {
			m.cursor++
			m.ensureCursorVisible()
		}

	case key.Matches(msg, m.keys.Toggle):
		if m.cursor < len(m.sections) {
			m.sections[m.cursor].Expanded = !m.sections[m.cursor].Expanded
		}

	case key.Matches(msg, m.keys.ExpandAll):
		for i := range m.sections {
			m.sections[i].Expanded = true
		}

	case key.Matches(msg, m.keys.CollapseAll):
		for i := range m.sections {
			m.sections[i].Expanded = false
		}

	case key.Matches(msg, m.keys.CopyID):
		if m.cursor < len(m.sections) {
			id := m.sections[m.cursor].ID
			// OSC52 escape sequence copies text to system clipboard.
			// Most modern terminals support this (iTerm2, Kitty, WezTerm,
			// Alacritty, etc.). It's like a terminal-native "copy to clipboard"
			// API — the terminal intercepts the escape sequence and copies
			// the base64-decoded payload.
			return m, copyToClipboard(id)
		}
	}

	return m, nil
}

// handleEvent routes typed events to the right section.
func (m *Model) handleEvent(e Event) {
	switch ev := e.(type) {

	case LogEvent:
		m.handleLogEvent(ev)

	case StartupEvent:
		m.handleStartupEvent(ev)

	case TurnStartEvent:
		m.handleTurnStartEvent(ev)

	case ContextEvent:
		m.appendToTurnGroup(ev.TurnID, "context", renderContextEvent(ev))

	case AgentIterEvent:
		m.appendToTurnGroup(ev.TurnID, "tools", renderAgentIterEvent(ev))

	case ToolCallEvent:
		m.appendToTurnGroup(ev.TurnID, "tools", renderToolCallEvent(ev))
		m.incrementTurnToolCount(ev.TurnID)

	case ReplyEvent:
		m.appendToTurnGroup(ev.TurnID, "reply", renderReplyEvent(ev))
		m.totalCost += ev.CostUSD

	case TurnEndEvent:
		m.handleTurnEndEvent(ev)

	case PersonaEvent:
		m.appendToTurnGroup(ev.TurnID, "persona", renderPersonaEvent(ev))

	case SidecarEvent:
		m.handleSidecarEvent(ev)
	}
}

// --- Event handlers ---

func (m *Model) handleLogEvent(ev LogEvent) {
	// LogEvents should only go to "startup" or "general" sections.
	// Turns have rich typed events (AgentIterEvent, ToolCallEvent, etc.)
	// that already cover the same data with better formatting.
	// Sidecars have their own SidecarEvents from pipe scanning.
	// Without this routing, LogEvents leak into whatever section happens
	// to be "last" — causing scheduler logs to appear in STT sections, etc.

	// During startup (before any turns), route to the startup section
	if m.messageCount == 0 {
		sec := m.ensureSection("startup", "Startup", "startup")
		sec.Lines = append(sec.Lines, renderLogEvent(ev))
		m.autoScroll()
		return
	}

	// After turns exist, suppress most LogEvents from the TUI.
	// They still go to her.log via the file logger subscriber.
	// Only show warnings and errors in a general section.
	if ev.Level >= LevelWarn {
		sec := m.ensureSection("general", "Logs", "general")
		sec.Lines = append(sec.Lines, renderLogEvent(ev))
		m.autoScroll()
	}
}

func (m *Model) handleStartupEvent(ev StartupEvent) {
	sec := m.ensureSection("startup", "Startup", "startup")
	line := renderStartupEvent(ev)
	sec.Lines = append(sec.Lines, line)
	m.autoScroll()
}

func (m *Model) handleTurnStartEvent(ev TurnStartEvent) {
	id := fmt.Sprintf("turn-%d", ev.TurnID)
	title := fmt.Sprintf("Turn #%d", m.messageCount+1)
	m.messageCount++

	sec := m.ensureSection(id, title, "turn")
	sec.Timestamp = ev.Time
	sec.TurnID = ev.TurnID
	sec.UserMessage = ev.UserMessage
	sec.Expanded = true // new turns start expanded so you see events arrive
	m.autoScroll()
}

func (m *Model) handleTurnEndEvent(ev TurnEndEvent) {
	sec := m.findSection(fmt.Sprintf("turn-%d", ev.TurnID))
	if sec == nil {
		return
	}
	sec.CostUSD = ev.TotalCost
	sec.LatencyMs = ev.ElapsedMs
	sec.ToolCount = ev.ToolCalls
	m.totalCost += ev.TotalCost
}

func (m *Model) handleSidecarEvent(ev SidecarEvent) {
	sec := m.ensureSection(ev.Sidecar, ev.Sidecar, "sidecar")
	sec.Lines = append(sec.Lines, renderSidecarEvent(ev))
}

// --- Section helpers ---

// ensureSection finds or creates a section by ID.
func (m *Model) ensureSection(id, title, kind string) *Section {
	for i := range m.sections {
		if m.sections[i].ID == id {
			return &m.sections[i]
		}
	}
	m.sections = append(m.sections, Section{
		ID:        id,
		Title:     title,
		Kind:      kind,
		Timestamp: time.Now(),
	})
	return &m.sections[len(m.sections)-1]
}

// findSection returns a pointer to a section by ID, or nil.
func (m *Model) findSection(id string) *Section {
	for i := range m.sections {
		if m.sections[i].ID == id {
			return &m.sections[i]
		}
	}
	return nil
}

// appendToTurnGroup adds a line to a specific content group within a turn section.
// Groups: "context", "tools", "reply", "persona"
func (m *Model) appendToTurnGroup(turnID int64, group, line string) {
	id := fmt.Sprintf("turn-%d", turnID)
	sec := m.findSection(id)
	if sec == nil && len(m.sections) > 0 {
		sec = &m.sections[len(m.sections)-1]
	}
	if sec == nil {
		return
	}
	switch group {
	case "context":
		sec.ContextLines = append(sec.ContextLines, line)
	case "tools":
		sec.ToolLines = append(sec.ToolLines, line)
	case "reply":
		sec.ReplyLines = append(sec.ReplyLines, line)
	case "persona":
		sec.PersonaLines = append(sec.PersonaLines, line)
	}
	m.autoScroll()
}

// incrementTurnToolCount bumps the tool count for a turn (used in collapsed header).
func (m *Model) incrementTurnToolCount(turnID int64) {
	sec := m.findSection(fmt.Sprintf("turn-%d", turnID))
	if sec != nil {
		sec.ToolCount++
	}
}

// autoScroll moves the view to the bottom when new content arrives,
// but only if we're already at/near the bottom (don't interrupt browsing).
func (m *Model) autoScroll() {
	totalLines := m.countRenderedLines()
	bodyHeight := m.bodyHeight()
	if bodyHeight <= 0 {
		return
	}
	// Auto-scroll if we're within 3 lines of the bottom
	maxScroll := totalLines - bodyHeight
	if maxScroll < 0 {
		maxScroll = 0
	}
	if m.scroll >= maxScroll-3 {
		m.scroll = maxScroll
	}
}

// ensureCursorVisible adjusts scroll so the cursor section is in view.
func (m *Model) ensureCursorVisible() {
	// Count lines before cursor section
	linesBefore := 0
	for i := 0; i < m.cursor && i < len(m.sections); i++ {
		linesBefore += m.sectionHeight(i)
	}
	cursorHeight := 0
	if m.cursor < len(m.sections) {
		cursorHeight = m.sectionHeight(m.cursor)
	}

	bodyH := m.bodyHeight()
	if bodyH <= 0 {
		return
	}

	// Scroll up if cursor is above viewport
	if linesBefore < m.scroll {
		m.scroll = linesBefore
	}
	// Scroll down if cursor is below viewport
	if linesBefore+cursorHeight > m.scroll+bodyH {
		m.scroll = linesBefore + cursorHeight - bodyH
	}
}

// sectionHeight returns how many rendered lines a section takes.
func (m *Model) sectionHeight(idx int) int {
	if idx >= len(m.sections) {
		return 0
	}
	sec := &m.sections[idx]
	if !sec.Expanded {
		return 1 // collapsed = one header line
	}
	if sec.Kind == "turn" {
		return 1 + m.turnContentHeight(sec)
	}
	return 1 + len(sec.Lines) // header + content lines
}

// turnContentHeight counts lines for a turn's grouped content.
// Each non-empty group renders as a bordered box: top border(1) + content lines + bottom border(1)
func (m *Model) turnContentHeight(sec *Section) int {
	height := 0
	for _, group := range [][]string{sec.ContextLines, sec.ToolLines, sec.ReplyLines, sec.PersonaLines} {
		if len(group) > 0 {
			// Border adds 2 lines (top + bottom). Content lines may wrap
			// but we estimate 1 terminal line per content line.
			height += len(group) + 2
		}
	}
	return height
}

// countRenderedLines returns the total number of visible lines across all sections.
func (m *Model) countRenderedLines() int {
	total := 0
	for i := range m.sections {
		total += m.sectionHeight(i)
	}
	return total
}

// bodyHeight returns the available lines for the sections viewport.
// Header is 2 lines, footer is 1 line.
func (m *Model) bodyHeight() int {
	h := m.height - 3 // 2 header + 1 footer
	if h < 1 {
		h = 1
	}
	return h
}

// copyToClipboard returns a tea.Cmd that writes an OSC52 escape sequence
// to copy text to the system clipboard.
func copyToClipboard(text string) tea.Cmd {
	return tea.SetClipboard(text)
}
