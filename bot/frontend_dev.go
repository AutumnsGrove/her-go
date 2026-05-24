package bot

import "sync"

// DevFrontend implements Frontend for the HTTP dev server.
// Instead of sending messages to Telegram, it collects the reply
// text so the HTTP handler can return it as JSON. Status updates
// go to the event bus (TUI shows them in the terminal).
//
// Think of this as the "null transport" — it captures output instead
// of routing it to a real messaging platform. Same idea as Python's
// io.StringIO capturing print() output for testing.
type DevFrontend struct {
	mu    sync.Mutex
	reply string
}

// NewDevFrontend creates a frontend that buffers replies for HTTP responses.
func NewDevFrontend() *DevFrontend {
	return &DevFrontend{}
}

func (f *DevFrontend) SendPlaceholder(text string, html bool) error { return nil }

func (f *DevFrontend) EditStatus(text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reply = text
	return nil
}

func (f *DevFrontend) SendReply(text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.reply != "" {
		f.reply += "\n\n"
	}
	f.reply += text
	return nil
}

func (f *DevFrontend) SendPaginated(text string) error {
	return f.SendReply(text)
}

func (f *DevFrontend) SendConfirm(text string) (int64, error) {
	// Dev mode auto-confirms. In a future version, the Gradio frontend
	// could show a confirmation dialog.
	return 0, nil
}

func (f *DevFrontend) StageReset() error      { return nil }
func (f *DevFrontend) DeletePlaceholder() error { return nil }

func (f *DevFrontend) StartTyping() func() {
	return func() {} // no-op — HTTP doesn't have typing indicators
}

func (f *DevFrontend) SupportsStreaming() bool   { return false }
func (f *DevFrontend) OnStreamToken(token string) {}
func (f *DevFrontend) StopStream()               {}

func (f *DevFrontend) SendBusy() error {
	return nil // HTTP will return 429 at the handler level
}

func (f *DevFrontend) SendError(text string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reply = text
	return nil
}

func (f *DevFrontend) ReplyText() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reply
}
