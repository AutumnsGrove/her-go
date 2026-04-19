package sim

import (
	"fmt"
	"sync"
	"time"
)

// Event is a single thing the bot would have shown the user, or a
// callback the user would have triggered by tapping an inline button.
// Every interaction flows through FakeTransport and lands in a slice of
// Events, in order. Tests can walk the slice to make assertions like
// "third message the user saw had an inline keyboard with 7 buttons."
type Event struct {
	Kind      EventKind
	Timestamp time.Time
	ChatID    int64

	// MessageID is the server-assigned ID of the message this event
	// refers to. Sends allocate a new ID; edits target an existing one;
	// callbacks fire on a specific message.
	MessageID int

	// Text holds the message body (for Send and Edit events).
	Text string

	// PNG holds the image bytes (for SendPNG). Kept in-memory so tests
	// can assert on size or content type without round-tripping bytes
	// through disk.
	PNG []byte

	// Caption is the caption on an image, if present.
	Caption string

	// Buttons is the flat list of inline-button labels on the message,
	// with their callback Data attached. Telegram's row/column structure
	// is collapsed for test ergonomics — use len(Buttons) to verify
	// count; use ButtonByLabel() to simulate a tap.
	Buttons []Button

	// Callback fields (only set on EventCallback).
	CallbackKind string // e.g. "mood", "confirm", "page"
	CallbackData string // e.g. "5" for "mood" action
}

// EventKind names what kind of interaction this Event records.
type EventKind string

const (
	EventSend     EventKind = "send"
	EventEdit     EventKind = "edit"
	EventSendPNG  EventKind = "send_png"
	EventCallback EventKind = "callback"
)

// Button describes a single inline keyboard button. Label is what the
// user reads; Data is the callback payload the handler receives when
// the button is tapped.
type Button struct {
	// Unique matches the telebot InlineButton.Unique field — the
	// registered callback handler is keyed on this (e.g. "mood",
	// "confirm").
	Unique string

	// Label is the human-visible text on the button.
	Label string

	// Data is the callback payload attached to the button (e.g. "5"
	// for a mood rating, "yes" for a confirmation).
	Data string
}

// FakeTransport implements just enough of the Telegram bot API surface
// for the sim: Send, Edit, SendPNG, and a test-only Dispatch() that
// simulates a user tapping an inline button.
//
// It is safe for concurrent use — the mood agent runs in a goroutine
// parallel to the main reply, so assertions may occur mid-flight.
type FakeTransport struct {
	mu        sync.Mutex
	clock     Clock
	nextMsgID int
	events    []Event

	// messages indexes every Send/Edit/SendPNG event by MessageID so
	// the most recent text for a given message can be looked up. This
	// is what the proposal-expiry sweeper would use to edit a message
	// in place: Send creates it, subsequent Edits overwrite Text.
	messages map[int]*Event
}

// NewFakeTransport constructs a FakeTransport bound to the given clock.
// The clock drives Event.Timestamp; in the sim we use a FakeClock so
// tests can compare timestamps deterministically.
func NewFakeTransport(clock Clock) *FakeTransport {
	return &FakeTransport{
		clock:     clock,
		nextMsgID: 1000, // start high so IDs don't look like array indices
		messages:  map[int]*Event{},
	}
}

// Send records a plain-text message the bot pushed to the user.
// Returns the allocated message ID so callers (e.g. the mood wizard)
// can later edit it.
func (ft *FakeTransport) Send(chatID int64, text string, buttons ...Button) (int, error) {
	ft.mu.Lock()
	defer ft.mu.Unlock()

	ft.nextMsgID++
	evt := Event{
		Kind:      EventSend,
		Timestamp: ft.clock.Now(),
		ChatID:    chatID,
		MessageID: ft.nextMsgID,
		Text:      text,
		Buttons:   append([]Button(nil), buttons...),
	}
	ft.events = append(ft.events, evt)
	stored := evt
	ft.messages[evt.MessageID] = &stored
	return evt.MessageID, nil
}

// Edit overwrites the body (and optionally the buttons) of a previously
// sent message. Returns an error if the message ID wasn't allocated by
// a prior Send.
func (ft *FakeTransport) Edit(chatID int64, msgID int, newText string, buttons ...Button) error {
	ft.mu.Lock()
	defer ft.mu.Unlock()

	base, ok := ft.messages[msgID]
	if !ok {
		return fmt.Errorf("fake transport: edit of unknown message %d", msgID)
	}

	evt := Event{
		Kind:      EventEdit,
		Timestamp: ft.clock.Now(),
		ChatID:    chatID,
		MessageID: msgID,
		Text:      newText,
		Buttons:   append([]Button(nil), buttons...),
	}
	ft.events = append(ft.events, evt)
	// Mirror the latest view into the indexed copy so ButtonByLabel
	// reflects the current state of the message.
	base.Text = newText
	base.Buttons = evt.Buttons
	return nil
}

// SendPNG records an image (e.g. a /mood week chart) the bot pushed.
// The bytes are stored verbatim — tests can assert len() > 0 or check
// that the PNG signature is present.
func (ft *FakeTransport) SendPNG(chatID int64, png []byte, caption string) error {
	ft.mu.Lock()
	defer ft.mu.Unlock()

	ft.nextMsgID++
	evt := Event{
		Kind:      EventSendPNG,
		Timestamp: ft.clock.Now(),
		ChatID:    chatID,
		MessageID: ft.nextMsgID,
		PNG:       append([]byte(nil), png...),
		Caption:   caption,
	}
	ft.events = append(ft.events, evt)
	stored := evt
	ft.messages[evt.MessageID] = &stored
	return nil
}

// Dispatch simulates the user tapping an inline button. It records the
// callback event and returns; the actual callback handler must be
// plumbed in by the scenario (typically via the real bot's callback
// dispatch function, which reads ft.events to find the target message
// and writes changes via Edit).
//
// In practice, scenarios call bot.HandleCallback(chatID, msgID, kind,
// data) directly; this method is here as a shorthand that also records
// the user-side action for later assertions.
func (ft *FakeTransport) Dispatch(chatID int64, msgID int, kind, data string) Event {
	ft.mu.Lock()
	defer ft.mu.Unlock()

	evt := Event{
		Kind:         EventCallback,
		Timestamp:    ft.clock.Now(),
		ChatID:       chatID,
		MessageID:    msgID,
		CallbackKind: kind,
		CallbackData: data,
	}
	ft.events = append(ft.events, evt)
	return evt
}

// Events returns a copy of every recorded event, oldest first.
func (ft *FakeTransport) Events() []Event {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	out := make([]Event, len(ft.events))
	copy(out, ft.events)
	return out
}

// LastMessage returns the most recent Send or Edit event, or nil if
// none exist. Useful for "the last thing the user saw was X"
// assertions.
func (ft *FakeTransport) LastMessage() *Event {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	for i := len(ft.events) - 1; i >= 0; i-- {
		e := ft.events[i]
		if e.Kind == EventSend || e.Kind == EventEdit || e.Kind == EventSendPNG {
			return &e
		}
	}
	return nil
}

// MessagesByKind filters recorded events by kind — handy for "how many
// inline-button proposals did we emit during this scenario".
func (ft *FakeTransport) MessagesByKind(kind EventKind) []Event {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	out := make([]Event, 0, len(ft.events))
	for _, e := range ft.events {
		if e.Kind == kind {
			out = append(out, e)
		}
	}
	return out
}

// ButtonByLabel looks up a button on the most recent view of a message.
// Returns nil if the message isn't tracked or no button matches.
// Scenarios use this to simulate taps by label rather than by
// inspecting raw keyboard structures.
func (ft *FakeTransport) ButtonByLabel(msgID int, label string) *Button {
	ft.mu.Lock()
	defer ft.mu.Unlock()
	msg, ok := ft.messages[msgID]
	if !ok {
		return nil
	}
	for i := range msg.Buttons {
		if msg.Buttons[i].Label == label {
			b := msg.Buttons[i]
			return &b
		}
	}
	return nil
}

// SendFunc adapts FakeTransport to the scheduler's Deps.Send signature
// (chatID, text → messageID, error). Scenarios wire this through when
// constructing scheduler.Deps so extensions that push proactive
// messages land in our event log.
func (ft *FakeTransport) SendFunc() func(chatID int64, text string) (int, error) {
	return func(chatID int64, text string) (int, error) {
		return ft.Send(chatID, text)
	}
}

// SendPNGFunc adapts FakeTransport to Deps.SendPNG.
func (ft *FakeTransport) SendPNGFunc() func(chatID int64, png []byte, caption string) error {
	return ft.SendPNG
}
