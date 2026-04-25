package instance

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Delta is the minimal shape channel adapters consume. The raw upstream line
// is kept so adapters can decide whether to forward or translate.
type Delta struct {
	Event string
	Data  json.RawMessage
}

// ProxySSE copies an SSE stream from src to w, flushing after every event.
// It returns when the stream ends, the context is cancelled, or w cannot be
// flushed (the client went away).
func ProxySSE(ctx context.Context, w http.ResponseWriter, src io.Reader) error {
	flusher, ok := w.(http.Flusher)
	if !ok {
		return errors.New("response writer does not support flushing")
	}
	setSSEHeaders(w.Header())
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	reader := bufio.NewReader(src)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if _, werr := w.Write(line); werr != nil {
				return werr
			}
			// An SSE event ends with a blank line; flush at that boundary and
			// also at every other line so consumers see chunks promptly.
			if len(line) == 1 || (len(line) == 2 && line[0] == '\r') {
				flusher.Flush()
			} else {
				flusher.Flush()
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		}
	}
}

// StreamDeltas parses an SSE stream and emits each event as a Delta on out.
// Closes out when the stream ends. Suitable for channel adapters that need
// typed access to upstream events.
func StreamDeltas(ctx context.Context, src io.Reader, out chan<- Delta) error {
	defer close(out)
	reader := bufio.NewReader(src)
	var event string
	var dataBuf []byte
	flush := func() error {
		if len(dataBuf) == 0 && event == "" {
			return nil
		}
		d := Delta{Event: event, Data: append([]byte(nil), dataBuf...)}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case out <- d:
		}
		event = ""
		dataBuf = dataBuf[:0]
		return nil
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		line, err := reader.ReadString('\n')
		if len(line) > 0 {
			trimmed := strings.TrimRight(line, "\r\n")
			switch {
			case trimmed == "":
				if ferr := flush(); ferr != nil {
					return ferr
				}
			case strings.HasPrefix(trimmed, "event:"):
				event = strings.TrimSpace(strings.TrimPrefix(trimmed, "event:"))
			case strings.HasPrefix(trimmed, "data:"):
				payload := strings.TrimPrefix(trimmed, "data:")
				payload = strings.TrimPrefix(payload, " ")
				if len(dataBuf) > 0 {
					dataBuf = append(dataBuf, '\n')
				}
				dataBuf = append(dataBuf, payload...)
				// Lines starting with ":" are SSE comments and are ignored
				// implicitly by falling through with no matching case.
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return flush()
			}
			return fmt.Errorf("read sse: %w", err)
		}
	}
}

func setSSEHeaders(h http.Header) {
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
}
