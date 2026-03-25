package tui

import "charm.land/lipgloss/v2"

// Color palette — using ANSI 256 colors for broad terminal compatibility.
// These look great on dark backgrounds (which most dev terminals use).
var (
	// Subtle grays for borders and secondary text
	colorDim    = lipgloss.Color("240") // gray
	colorSubtle = lipgloss.Color("245") // lighter gray
	colorBorder = lipgloss.Color("238") // dark gray for borders

	// Semantic colors
	colorInfo    = lipgloss.Color("75")  // soft blue
	colorWarn    = lipgloss.Color("214") // amber/orange
	colorError   = lipgloss.Color("196") // red
	colorSuccess = lipgloss.Color("78")  // green
	colorCost    = lipgloss.Color("220") // gold/yellow
	colorCyan    = lipgloss.Color("80")  // teal for tools
	colorPurple  = lipgloss.Color("141") // lavender for persona events

	// Section-specific
	colorStartup = lipgloss.Color("213") // pink
	colorTurn    = lipgloss.Color("75")  // blue
	colorSidecar = lipgloss.Color("245") // gray (less important)
)

// --- Header ---

// headerStyle is the top bar showing uptime, message count, total cost.
var headerStyle = lipgloss.NewStyle().
	Bold(true).
	Foreground(lipgloss.Color("255")).
	Background(lipgloss.Color("236")).
	Padding(0, 1)

var headerTitleStyle = lipgloss.NewStyle().
	Bold(true).
	Foreground(lipgloss.Color("213")) // pink "her"

var headerStatStyle = lipgloss.NewStyle().
	Foreground(colorSubtle)

var headerStatValueStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color("255"))

// --- Footer ---

var footerStyle = lipgloss.NewStyle().
	Foreground(colorDim)

var footerKeyStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color("255")).
	Bold(true)

var footerDescStyle = lipgloss.NewStyle().
	Foreground(colorDim)

// --- Section headers ---

// sectionHeaderStyle is the base for collapsed/expanded section headers.
var sectionHeaderStyle = lipgloss.NewStyle().
	Bold(true)

var sectionCursorStyle = lipgloss.NewStyle().
	Foreground(lipgloss.Color("213")).
	Bold(true)

var sectionTimestampStyle = lipgloss.NewStyle().
	Foreground(colorDim)

var sectionMetricsStyle = lipgloss.NewStyle().
	Foreground(colorCost)

// --- Section content (expanded) ---

// sectionBorderStyle wraps expanded content in a rounded box.
var sectionBorderStyle = lipgloss.NewStyle().
	Border(lipgloss.RoundedBorder()).
	BorderForeground(colorBorder).
	Padding(0, 1)

// --- Log event styling by level ---

var logDebugStyle = lipgloss.NewStyle().Foreground(colorDim)
var logInfoStyle = lipgloss.NewStyle().Foreground(colorSubtle)
var logWarnStyle = lipgloss.NewStyle().Foreground(colorWarn)
var logErrorStyle = lipgloss.NewStyle().Foreground(colorError)
var logFatalStyle = lipgloss.NewStyle().Foreground(colorError).Bold(true)

// --- Tool call styling ---

var toolNameStyle = lipgloss.NewStyle().
	Foreground(colorCyan).
	Bold(true)

var toolResultStyle = lipgloss.NewStyle().
	Foreground(colorSubtle)

var toolArrowStyle = lipgloss.NewStyle().
	Foreground(colorDim)

// --- Think styling ---

var thinkStyle = lipgloss.NewStyle().
	Foreground(colorDim).
	Italic(true)

// --- Reply styling ---

var replyTextStyle = lipgloss.NewStyle().
	Foreground(colorSuccess)

var replyMetricsStyle = lipgloss.NewStyle().
	Foreground(colorCost)

// --- Persona styling ---

var personaStyle = lipgloss.NewStyle().
	Foreground(colorPurple)

// --- Sub-section headers inside expanded turns ---

var subHeaderStyle = lipgloss.NewStyle().
	Foreground(colorDim).
	Bold(true)
