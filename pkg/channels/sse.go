package channels

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// SetSSEHeaders sets the headers for a Server-Sent Events response stream.
func SetSSEHeaders(h http.Header) {
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
}

// WriteSSEError emits err as an SSE error event and flushes it.
func WriteSSEError(w http.ResponseWriter, flusher http.Flusher, err error) {
	_, _ = fmt.Fprintf(w, "event: error\ndata: %q\n\n", err.Error())
	flusher.Flush()
}

// WriteJSONError writes an HTTP error as a JSON body with the given status code.
func WriteJSONError(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": http.StatusText(code)},
	})
}

// StreamDeltasSSE relays an OutboundDelta channel to an already-opened SSE
// response: each content delta is one `data:` event, the terminal delta is a
// `done` event, and an error delta is an `error` event that ends the stream.
// The caller sets its own headers (SetSSEHeaders) and writes the status before
// calling this.
func StreamDeltasSSE(w http.ResponseWriter, flusher http.Flusher, deltas <-chan OutboundDelta) {
	enc := json.NewEncoder(w)
	for d := range deltas {
		if d.Err != nil {
			WriteSSEError(w, flusher, d.Err)
			return
		}
		if d.Done {
			_, _ = fmt.Fprintf(w, "event: done\ndata: {}\n\n")
			flusher.Flush()
			continue
		}
		if d.Content == "" {
			continue
		}
		if _, err := io.WriteString(w, "data: "); err != nil {
			return
		}
		if err := enc.Encode(map[string]string{"content": d.Content}); err != nil {
			return
		}
		// enc.Encode writes a trailing newline; SSE needs the blank line after.
		if _, err := io.WriteString(w, "\n"); err != nil {
			return
		}
		flusher.Flush()
	}
}
