package gateway

import (
	_ "embed"
	"net/http"
)

//go:embed chat.html
var chatHTML []byte

func (a *gradioAdapter) handleUI(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Write(chatHTML)
}
