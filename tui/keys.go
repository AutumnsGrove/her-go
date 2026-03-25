package tui

import "charm.land/bubbles/v2/key"

// KeyMap defines all keyboard shortcuts for the TUI.
// This is the standard Bubble Tea pattern for keybindings — you define them
// as key.Binding values and match against them in Update().
//
// The bubbles/key package handles matching across different terminal
// representations of the same key (e.g., "up" and "k" both mean "move up").
type KeyMap struct {
	Up          key.Binding
	Down        key.Binding
	Toggle      key.Binding // enter — expand/collapse current section
	ExpandAll   key.Binding
	CollapseAll key.Binding
	CopyID      key.Binding
	Quit        key.Binding
	Help        key.Binding
}

// DefaultKeyMap returns the default keybindings. These are displayed
// in the footer help bar.
func DefaultKeyMap() KeyMap {
	return KeyMap{
		Up: key.NewBinding(
			key.WithKeys("up", "k"),
			key.WithHelp("↑/k", "up"),
		),
		Down: key.NewBinding(
			key.WithKeys("down", "j"),
			key.WithHelp("↓/j", "down"),
		),
		Toggle: key.NewBinding(
			key.WithKeys("enter", " "),
			key.WithHelp("enter", "toggle"),
		),
		ExpandAll: key.NewBinding(
			key.WithKeys("e"),
			key.WithHelp("e", "expand all"),
		),
		CollapseAll: key.NewBinding(
			key.WithKeys("c"),
			key.WithHelp("c", "collapse all"),
		),
		CopyID: key.NewBinding(
			key.WithKeys("y"),
			key.WithHelp("y", "copy ID"),
		),
		Quit: key.NewBinding(
			key.WithKeys("q", "ctrl+c"),
			key.WithHelp("q", "quit"),
		),
		Help: key.NewBinding(
			key.WithKeys("?"),
			key.WithHelp("?", "help"),
		),
	}
}
