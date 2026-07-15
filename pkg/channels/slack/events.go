package slack

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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

	// Slack redelivers an event up to 3 times when the ack is slow or fails.
	// Handling is asynchronous (ack-first), so a duplicate delivery would start
	// a duplicate turn; handleInbound dedups on event_id for both transports. A
	// retry without an event_id cannot be deduped there, so it is dropped here:
	// it cannot be distinguished from an already-handled delivery.
	if r.Header.Get("X-Slack-Retry-Num") != "" && env.EventID == "" {
		h.logger.Info("slack: dropping redelivered event without event_id",
			"retry_num", r.Header.Get("X-Slack-Retry-Num"),
			"retry_reason", r.Header.Get("X-Slack-Retry-Reason"))
		w.Header().Set("X-Slack-No-Retry", "1")
		w.WriteHeader(http.StatusOK)
		return
	}

	// Acknowledge immediately; Slack requires a 200 within 3 seconds.
	w.WriteHeader(http.StatusOK)

	if env.Type == "event_callback" && env.Event != nil {
		ev := *env.Event
		go h.adapter.handleInbound(h.ctx, ev, env.EventID)
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
	EventID   string           `json:"event_id,omitempty"`
	Event     *slackInnerEvent `json:"event,omitempty"`
}

// seenEventTTL bounds how long delivered event IDs are remembered for dedup.
// Slack redelivers within roughly an hour at most; entries past the TTL are
// swept opportunistically on insert.
const seenEventTTL = time.Hour

// seenEvent records eventID as delivered and reports whether it had already
// been recorded, so duplicate Events API deliveries are processed exactly once.
func (a *Adapter) seenEvent(eventID string) bool {
	now := time.Now()
	a.seenEventsMu.Lock()
	defer a.seenEventsMu.Unlock()
	if expiry, ok := a.seenEvents[eventID]; ok && now.Before(expiry) {
		return true
	}
	if a.seenEvents == nil {
		a.seenEvents = make(map[string]time.Time)
	}
	for id, expiry := range a.seenEvents {
		if now.After(expiry) {
			delete(a.seenEvents, id)
		}
	}
	a.seenEvents[eventID] = now.Add(seenEventTTL)
	return false
}
