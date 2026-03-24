// keyboard.go — Telegram-agnostic inline keyboard types.
//
// These types let the scheduler (and future callers) describe
// interactive button layouts without importing any Telegram library.
// The bot package translates these into real telebot types.
//
// The key mapping:
//   Button.Action → telebot's InlineButton.Unique (routes callbacks)
//   Button.Value  → telebot's InlineButton.Data (payload)
//   Button.Text   → telebot's InlineButton.Text (display)
//
// This is the same dependency inversion pattern as SendFunc — the
// scheduler depends on types it defines, not types from the bot.
package scheduler

// Button describes a single inline keyboard button.
//
// Action is the routing key that determines which callback handler
// fires when the button is clicked. Think of it as the "endpoint"
// name. Value is the data that handler receives — it can be anything
// the handler knows how to parse.
//
// Examples:
//
//	Button{Text: "😊 Great", Action: "mood", Value: "5"}
//	Button{Text: "✅ Yes",   Action: "med",  Value: "yes"}
//	Button{Text: "⏰ Snooze", Action: "med", Value: "snooze"}
type Button struct {
	Text   string // what the user sees on the button
	Action string // callback routing key (e.g., "mood", "med", "confirm")
	Value  string // payload data the handler receives (e.g., "5", "yes")
}

// KeyboardRow is a horizontal row of buttons. Telegram displays these
// side by side. Keep rows to 3-5 buttons max — more than that gets
// cramped on phone screens.
type KeyboardRow []Button

// InlineKeyboard is a complete keyboard layout: a slice of rows.
// Each row is displayed on its own line in the Telegram message.
type InlineKeyboard []KeyboardRow

// KeyboardMessage bundles text + an inline keyboard for sending.
// This is what the scheduler passes to SendKeyboardFunc — the bot
// translates it into a real Telegram message with buttons.
type KeyboardMessage struct {
	Text     string         // message text (supports HTML)
	Keyboard InlineKeyboard // button layout
}

// SendKeyboardFunc sends a message with an inline keyboard attached.
// Same dependency inversion as SendFunc — the scheduler calls this
// function, and the bot provides the concrete implementation as a
// closure wired in cmd/run.go.
type SendKeyboardFunc func(msg KeyboardMessage) error
