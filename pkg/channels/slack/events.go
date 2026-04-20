package slack

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

const signatureTimestampTolerance = 5 * time.Minute

// eventsHandler serves Slack Events API POST requests at /channels/slack/events.
type eventsHandler struct {
	signingSecret string
	botToken      string
	adapter       *Adapter
	logger        *slog.Logger
	// ctx is the adapter-lifecycle context, used for async dispatch goroutines
	// so they are not tied to the short-lived HTTP request context.
	ctx context.Context //nolint:containedctx
}

func (h *eventsHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}

	if err := VerifySignature(h.signingSecret, r.Header, body); err != nil {
		h.logger.Warn("slack: invalid request signature", "error", err)
		http.Error(w, "invalid signature", http.StatusUnauthorized)
		return
	}

	var env eventEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		http.Error(w, "invalid JSON", http.StatusBadRequest)
		return
	}

	// URL verification handshake (required during Slack app setup).
	if env.Type == "url_verification" {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{"challenge": env.Challenge})
		return
	}

	// Acknowledge immediately; Slack requires a 200 within 3 seconds.
	w.WriteHeader(http.StatusOK)

	if env.Type == "event_callback" && env.Event != nil {
		ev := *env.Event
		go func() {
			msg, ok := ev.toInboundMessage()
			if !ok {
				return
			}
			if err := h.adapter.dispatch(h.ctx, msg, ev.Channel, ev.TS); err != nil {
				if !errors.Is(err, context.Canceled) {
					h.logger.Error("slack events: dispatch error", "channel", ev.Channel, "error", err)
				}
			}
		}()
	}
}

// VerifySignature validates an inbound request using the Slack signing secret
// and the x-slack-signature / x-slack-request-timestamp headers.
// Exported so tests can call it directly.
func VerifySignature(signingSecret string, header http.Header, body []byte) error {
	sig := header.Get("X-Slack-Signature")
	ts := header.Get("X-Slack-Request-Timestamp")
	if sig == "" || ts == "" {
		return fmt.Errorf("missing signature headers")
	}

	tsInt, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return fmt.Errorf("invalid timestamp %q: %w", ts, err)
	}
	age := time.Since(time.Unix(tsInt, 0))
	if age < -signatureTimestampTolerance || age > signatureTimestampTolerance {
		return fmt.Errorf("timestamp out of tolerance window: age=%v", age)
	}

	base := "v0:" + ts + ":" + string(body)
	mac := hmac.New(sha256.New, []byte(signingSecret))
	_, _ = mac.Write([]byte(base))
	expected := "v0=" + hex.EncodeToString(mac.Sum(nil))

	if !hmac.Equal([]byte(expected), []byte(sig)) {
		return fmt.Errorf("signature mismatch")
	}
	return nil
}

// eventEnvelope is the top-level Slack Events API payload shape.
type eventEnvelope struct {
	Type      string           `json:"type"`
	Challenge string           `json:"challenge,omitempty"`
	Event     *slackInnerEvent `json:"event,omitempty"`
}
