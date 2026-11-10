package reviews

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"strings"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/auth/satoken"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// A caller makes a request safe to send again by giving it an Idempotency-Key
// (draft-ietf-httpapi-idempotency-key-header). The first request with a key
// claims it in the store, runs, and keeps its answer under it; the same request
// sent again with the key is answered from there and posts nothing, and
// another request with the key is refused. A key is its caller's own: two
// ServiceAccounts never share one.
const (
	idempotencyKeyHeader      = "Idempotency-Key"
	idempotencyReplayedHeader = "Idempotency-Replayed"
	// idempotencyKeyMax bounds a key, in bytes.
	idempotencyKeyMax = 128
	// idempotencyDocs is the published idempotency specification the draft
	// asks a resource for; every refusal's problem type points at it.
	idempotencyDocs = "https://github.com/giantswarm/klaus-gateway/blob/main/docs/api.md#retries-idempotency-key"
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
		fieldLines := r.Header.Values(idempotencyKeyHeader)
		if len(fieldLines) == 0 {
			next(w, r, caller)
			return
		}
		key, err := parseIdempotencyKey(strings.Join(fieldLines, ", "))
		if err != nil {
			writeProblem(w, http.StatusBadRequest, "Idempotency-Key is invalid", err.Error())
			return
		}
		h.once(w, r, caller, key, next)
	}
}

// once runs next for the first request with key and answers every later one
// from its receipt. The claim is a compare-and-set in the store, so of
// requests sent at once, on any replica, one runs and the others are told it
// is running.
func (h *Handler) once(w http.ResponseWriter, r *http.Request, caller satoken.Identity, key string, next callerHandler) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "invalid body: "+err.Error(), http.StatusBadRequest)
		return
	}
	id := receiptID(caller, key)
	claim := store.Receipt{Fingerprint: requestFingerprint(r.Method, r.URL.EscapedPath(), body), ClaimedAt: time.Now(), TTL: receiptTTL}
	var held store.Receipt
	claimed := false
	// Detached like the run: a claim written as the caller hangs up must be
	// answered, or it holds the key with no answer until it is abandoned.
	err = h.Receipts.UpdateReceipt(context.WithoutCancel(r.Context()), id, func(stored *store.Receipt, found bool) bool {
		claimed = !found || (stored.Fingerprint == claim.Fingerprint && stored.Status == 0 && claim.ClaimedAt.Sub(stored.ClaimedAt) > claimAbandoned)
		if !claimed {
			held = *stored
			return false
		}
		*stored = claim
		return true
	})
	switch {
	case err != nil:
		h.Logger.Error("idempotency: the key could not be claimed", "record", "idempotency_store_failed", "caller", caller.Username, "path", r.URL.Path, "error", err)
		writeProblem(w, http.StatusServiceUnavailable, "Idempotency-Key could not be checked", "the routing store did not answer; send the request again")
	case claimed:
		h.run(w, r, caller, id, claim, body, next)
	case held.Fingerprint != claim.Fingerprint:
		h.Logger.Warn("idempotency: the key was sent with a different request", "record", "idempotency_key_reused", "caller", caller.Username, "path", r.URL.Path)
		writeProblem(w, http.StatusUnprocessableEntity, "Idempotency-Key is already used", "this Idempotency-Key was sent with another method, path or body")
	case held.Status == 0:
		h.Logger.Info("idempotency: the request with the key is still running", "record", "idempotency_outstanding", "caller", caller.Username, "path", r.URL.Path)
		w.Header().Set("Retry-After", "1")
		writeProblem(w, http.StatusConflict, "A request is outstanding for this Idempotency-Key", "the first request with this Idempotency-Key is still running; send it again in a moment")
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
	detached := r.WithContext(ctx)
	detached.Body = io.NopCloser(bytes.NewReader(body))
	recorded := &recorder{header: http.Header{}}
	next(recorded, detached, caller)
	succeeded := recorded.status >= 200 && recorded.status < 300
	err := h.Receipts.UpdateReceipt(context.WithoutCancel(r.Context()), id, func(stored *store.Receipt, found bool) bool {
		if !found || stored.Fingerprint != claim.Fingerprint || !stored.ClaimedAt.Equal(claim.ClaimedAt) {
			return false
		}
		if succeeded {
			stored.Status, stored.Body = recorded.status, recorded.body.String()
		} else {
			*stored = store.Receipt{}
		}
		return true
	})
	if err != nil {
		h.Logger.Error("idempotency: the answer could not be kept", "record", "idempotency_store_failed", "caller", caller.Username, "path", r.URL.Path, "status", recorded.status, "error", err)
	}
	recorded.writeTo(w)
}

// receiptID names the receipt of key sent by caller. The ServiceAccount's UID
// is part of it, so a ServiceAccount deleted and created again under the same
// name does not inherit the receipts of the one before.
func receiptID(caller satoken.Identity, key string) string {
	sum := sha256.Sum256([]byte(caller.Username + "\n" + caller.UID + "\n" + key))
	return hex.EncodeToString(sum[:])
}

// requestFingerprint is the draft's idempotency fingerprint: a digest of the
// request's method, path and body.
func requestFingerprint(method, path string, body []byte) string {
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

// parseIdempotencyKey parses the field value as an RFC 9651 Item whose bare
// item is a String (RFC 9651 section 4.2.5), as the draft requires. Parameters
// are refused: the draft defines none. The key is 1 to idempotencyKeyMax bytes.
func parseIdempotencyKey(value string) (string, error) {
	value = strings.Trim(value, " ")
	if !strings.HasPrefix(value, `"`) {
		return "", fmt.Errorf("%s must be a quoted Structured Field String, such as \"review-4711-1\"", idempotencyKeyHeader)
	}
	var key strings.Builder
	for i := 1; i < len(value); i++ {
		c := value[i]
		switch {
		case c == '\\':
			i++
			if i == len(value) || (value[i] != '"' && value[i] != '\\') {
				return "", fmt.Errorf("%s has an invalid escape", idempotencyKeyHeader)
			}
			key.WriteByte(value[i])
		case c == '"':
			if i != len(value)-1 {
				return "", fmt.Errorf("%s must be a single String with no parameters", idempotencyKeyHeader)
			}
			if key.Len() == 0 || key.Len() > idempotencyKeyMax {
				return "", fmt.Errorf("%s must be 1 to %d bytes", idempotencyKeyHeader, idempotencyKeyMax)
			}
			return key.String(), nil
		case c < 0x20 || c > 0x7e:
			return "", fmt.Errorf("%s may hold printable ASCII only", idempotencyKeyHeader)
		default:
			key.WriteByte(c)
		}
	}
	return "", fmt.Errorf("%s has no closing quote", idempotencyKeyHeader)
}

// problem is an RFC 9457 problem detail, the shape of the draft's refusals.
type problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Detail string `json:"detail"`
}

func writeProblem(w http.ResponseWriter, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(problem{Type: idempotencyDocs, Title: title, Status: status, Detail: detail})
}
