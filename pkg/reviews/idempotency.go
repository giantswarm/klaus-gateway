package reviews

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"maps"
	"net/http"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/auth/satoken"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// A caller makes a request safe to send again by giving it an Idempotency-Key.
// The first request with a key claims it in the store, runs, and keeps its
// answer under it; the same request sent again with the key is answered from
// there and posts nothing, and another request with the key is refused. A key
// is its caller's own: two ServiceAccounts never share one.
const (
	idempotencyKeyHeader      = "Idempotency-Key"
	idempotencyReplayedHeader = "Idempotency-Replayed"
	// idempotencyKeyMax bounds a key, in bytes.
	idempotencyKeyMax = 128
	// receiptTTL is how long the answer to a request with a key is kept.
	receiptTTL = 7 * 24 * time.Hour
	// claimRun bounds the request that holds a key. A claim still without an
	// answer after claimAbandoned was left by a gateway that stopped before
	// answering, and the request runs again: claimAbandoned leaves room above
	// claimRun for saving the answer and for replicas whose clocks differ.
	claimRun       = 2 * time.Minute
	claimAbandoned = 5 * time.Minute
)

// callerHandler serves one route for an authenticated caller.
type callerHandler func(w http.ResponseWriter, r *http.Request, caller satoken.Identity)

// serve authenticates the caller, then runs next: once per Idempotency-Key
// when the request carries one.
func (h *Handler) serve(next callerHandler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		caller, ok := h.authenticate(w, r)
		if !ok {
			return
		}
		if key := r.Header.Get(idempotencyKeyHeader); key != "" {
			h.once(w, r, caller, key, next)
			return
		}
		next(w, r, caller)
	}
}

// once runs next for the first request with key and answers every later one
// from its receipt. The claim is a compare-and-set in the store, so of
// requests sent at once, on any replica, one runs and the others are told it
// is running.
func (h *Handler) once(w http.ResponseWriter, r *http.Request, caller satoken.Identity, key string, next callerHandler) {
	if len(key) > idempotencyKeyMax {
		http.Error(w, fmt.Sprintf("%s: at most %d bytes", idempotencyKeyHeader, idempotencyKeyMax), http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	id := receiptID(caller.Username, key)
	claim := store.Receipt{Hash: requestHash(r.Method, r.URL.EscapedPath(), body), ClaimedAt: time.Now(), TTL: receiptTTL}
	var held store.Receipt
	claimed := false
	// Detached like the run: a claim written as the caller hangs up must be
	// answered, or it holds the key with no answer until it is abandoned.
	err = h.Receipts.UpdateReceipt(context.WithoutCancel(r.Context()), id, func(rc *store.Receipt, found bool) bool {
		claimed = !found || (rc.Hash == claim.Hash && rc.Status == 0 && claim.ClaimedAt.Sub(rc.ClaimedAt) > claimAbandoned)
		if !claimed {
			held = *rc
			return false
		}
		*rc = claim
		return true
	})
	switch {
	case err != nil:
		h.Logger.Error("idempotency: the key could not be claimed", "record", "idempotency_store_failed", "caller", caller.Username, "path", r.URL.Path, "error", err)
		http.Error(w, "the Idempotency-Key could not be checked; send the request again", http.StatusServiceUnavailable)
	case claimed:
		h.run(w, r, caller, id, claim, body, next)
	case held.Hash != claim.Hash:
		h.Logger.Warn("idempotency: the key was sent with a different request", "record", "idempotency_conflict", "caller", caller.Username, "path", r.URL.Path)
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this Idempotency-Key was sent with a different request"})
	case held.Status == 0:
		h.Logger.Info("idempotency: the request with the key is still running", "record", "idempotency_in_flight", "caller", caller.Username, "path", r.URL.Path)
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "the request with this Idempotency-Key is still running; send it again in a moment"})
	default:
		h.Logger.Info("idempotency: answered from the receipt", "record", "idempotency_replayed", "caller", caller.Username, "path", r.URL.Path, "status", held.Status)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set(idempotencyReplayedHeader, "true")
		w.WriteHeader(held.Status)
		_, _ = io.WriteString(w, held.Body)
	}
}

// run serves the request that claimed the key, its body read already, and
// keeps its answer: a success under the key for receiptTTL; any other answer
// frees the key, since the request did not go through and may be sent again.
// The request keeps running when its caller hangs up, so that the caller's
// retry finds the answer instead of posting a second time.
func (h *Handler) run(w http.ResponseWriter, r *http.Request, caller satoken.Identity, id string, claim store.Receipt, body []byte, next callerHandler) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), claimRun)
	defer cancel()
	req := r.WithContext(ctx)
	req.Body = io.NopCloser(bytes.NewReader(body))
	rec := &recorder{header: http.Header{}}
	next(rec, req, caller)
	succeeded := rec.status >= 200 && rec.status < 300
	err := h.Receipts.UpdateReceipt(context.WithoutCancel(r.Context()), id, func(rc *store.Receipt, found bool) bool {
		if !found || rc.Hash != claim.Hash || !rc.ClaimedAt.Equal(claim.ClaimedAt) {
			return false
		}
		if succeeded {
			rc.Status, rc.Body = rec.status, rec.body.String()
		} else {
			*rc = store.Receipt{}
		}
		return true
	})
	if err != nil {
		h.Logger.Error("idempotency: the answer could not be kept", "record", "idempotency_store_failed", "caller", caller.Username, "path", r.URL.Path, "status", rec.status, "error", err)
	}
	rec.writeTo(w)
}

// receiptID names the receipt of key sent by caller.
func receiptID(caller, key string) string {
	sum := sha256.Sum256([]byte(caller + "\n" + key))
	return hex.EncodeToString(sum[:])
}

// requestHash identifies a request by its method, path and body.
func requestHash(method, path string, body []byte) string {
	h := sha256.New()
	_, _ = io.WriteString(h, method+" "+path+"\n")
	_, _ = h.Write(body)
	return hex.EncodeToString(h.Sum(nil))
}

// recorder holds the answer of the request that claimed a key until the
// answer is kept, then writes it out.
type recorder struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (rec *recorder) Header() http.Header { return rec.header }

func (rec *recorder) WriteHeader(status int) {
	if rec.status == 0 {
		rec.status = status
	}
}

func (rec *recorder) Write(b []byte) (int, error) {
	rec.WriteHeader(http.StatusOK)
	return rec.body.Write(b)
}

func (rec *recorder) writeTo(w http.ResponseWriter) {
	maps.Copy(w.Header(), rec.header)
	if rec.status != 0 {
		w.WriteHeader(rec.status)
	}
	_, _ = w.Write(rec.body.Bytes())
}
