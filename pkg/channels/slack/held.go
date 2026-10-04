package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"slices"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// heldState is what the adapter holds for one thread between turns, persisted
// in the thread's row (store.Entry.Held) so a gateway restart loses none of
// it: the messages parked while their sender signs in (Login) or while the
// initiator decides on them (Access), each by sender in arrival order, and the
// task paused on a prompt (Pending). Every entry keeps the time it was stored,
// so pendingTTL runs on across a restart.
//
// The process's maps stay the working copy; the row is written after every
// change to them and read back once at start (restoreHeld).
type heldState struct {
	Login   map[string][]heldMessage `json:"login,omitempty"`
	Access  map[string][]heldMessage `json:"access,omitempty"`
	Pending *heldTask                `json:"pending,omitempty"`
}

// heldMessage is a parked message as the row keeps it.
type heldMessage struct {
	Msg          channels.InboundMessage `json:"msg"`
	SlackChannel string                  `json:"slack_channel"`
	StoredAt     time.Time               `json:"stored_at"`
}

// heldTask is a paused task as the row keeps it.
type heldTask struct {
	Task     pendingTask `json:"task"`
	StoredAt time.Time   `json:"stored_at"`
}

func (h heldState) empty() bool {
	return len(h.Login) == 0 && len(h.Access) == 0 && h.Pending == nil
}

// marshal encodes h for the row; nil when it holds nothing, so the row drops
// the field.
func (h heldState) marshal() (json.RawMessage, error) {
	if h.empty() {
		return nil, nil
	}
	return json.Marshal(h)
}

// heldMessageOf is msg as the row keeps it: without the tokens a parked
// message never needs (the replay mints its own) and without attachment bytes
// the replay downloads again from their source.
func heldMessageOf(msg channels.InboundMessage, slackChannel string, storedAt time.Time) heldMessage {
	msg.BearerToken, msg.OwnerToken = "", ""
	msg.Attachments = slices.Clone(msg.Attachments)
	for i := range msg.Attachments {
		if msg.Attachments[i].SourceURL != "" {
			msg.Attachments[i].Bytes = nil
		}
	}
	return heldMessage{Msg: msg, SlackChannel: slackChannel, StoredAt: storedAt}
}

// heldSnapshot collects what the adapter holds for threadID now.
func (a *Adapter) heldSnapshot(threadID string) heldState {
	var h heldState
	a.pendingLoginMu.Lock()
	for user, threads := range a.pendingLogin {
		for _, r := range threads[threadID] {
			if h.Login == nil {
				h.Login = make(map[string][]heldMessage)
			}
			h.Login[user] = append(h.Login[user], heldMessageOf(r.msg, r.slackChannel, r.storedAt))
		}
	}
	a.pendingLoginMu.Unlock()

	a.pendingAccessMu.Lock()
	for user, queue := range a.pendingAccess[threadID] {
		for _, r := range queue {
			if h.Access == nil {
				h.Access = make(map[string][]heldMessage)
			}
			h.Access[user] = append(h.Access[user], heldMessageOf(r.msg, r.slackChannel, r.storedAt))
		}
	}
	a.pendingAccessMu.Unlock()

	a.threadsMu.Lock()
	if st := a.threads[threadID]; st != nil && st.pending != nil {
		h.Pending = &heldTask{Task: *st.pending, StoredAt: st.pending.storedAt}
	}
	a.threadsMu.Unlock()
	return h
}

// persistHeld writes what the adapter holds for threadID into the thread's
// row, keyed like the access policy's (slackChannel, threadID). It runs in
// the background — a store write is no reason to delay the Slack round-trip
// that changed the state — and snapshots the state inside the store's
// per-key update, so writes that finish out of order still leave the latest
// state in the row. Until restoreHeld has read the rows back, writes wait for
// it: an early write would replace a row's held state before it was read.
func (a *Adapter) persistHeld(slackChannel, threadID string) {
	if a.gw == nil || slackChannel == "" || threadID == "" {
		return
	}
	restored := a.heldRestored.Load()
	a.background(func(ctx context.Context) {
		if restored != nil {
			select {
			case <-*restored:
			case <-ctx.Done():
				return
			}
		}
		err := a.gw.UpdateThreadRecord(ctx, ChannelName, slackChannel, threadID, func(e *store.Entry, _ bool) bool {
			held, err := a.heldSnapshot(threadID).marshal()
			if err != nil {
				a.Logger.Warn("slack: encode held thread state failed", "thread", threadID, "error", err)
				return false
			}
			if bytes.Equal(held, e.Held) {
				return false
			}
			e.Held = held
			return true
		})
		if err != nil {
			a.Logger.Warn("slack: held thread state not persisted; a restart would lose it", "thread", threadID, "error", err)
		}
	})
}

// restoreHeld reads back what a previous process held for each live thread:
// the parked messages and the paused prompts, minus what pendingTTL expired
// meanwhile. A message parked here since the start is kept after the restored
// ones, within maxParkedPerThread. A user whose sign-in completed while
// nobody held their messages (on the previous process, or on this one before
// the read) is linked already: their messages replay now.
func (a *Adapter) restoreHeld(ctx context.Context) {
	// However the read ends, the writes held back meanwhile go ahead after
	// it; those of restored threads bring their rows to the merged state.
	if ch := a.heldRestored.Load(); ch != nil {
		defer close(*ch)
	}
	var rows []store.KeyEntry
	for again := recoverBackoff(); ; {
		lctx, cancel := context.WithTimeout(ctx, recoverListTimeout)
		list, err := a.gw.ThreadRecords(lctx, ChannelName)
		cancel()
		if err == nil {
			rows = list
			break
		}
		a.Logger.Warn("slack: held thread state unavailable", "error", err)
		if !again(ctx) {
			a.Logger.Warn("slack: held thread state not restored; messages parked before the restart are not replayed")
			return
		}
	}

	now := time.Now()
	fresh := func(at time.Time) bool { return now.Sub(at) <= pendingTTL }
	linkUsers := map[string]bool{}
	var threads []store.Key
	for _, row := range rows {
		if len(row.Entry.Held) == 0 {
			continue
		}
		var h heldState
		if err := json.Unmarshal(row.Entry.Held, &h); err != nil {
			a.Logger.Warn("slack: held thread state unreadable, dropped", "thread", row.Key.ThreadID, "error", err)
			continue
		}
		threadID := row.Key.ThreadID
		threads = append(threads, row.Key)

		a.pendingLoginMu.Lock()
		for user, held := range h.Login {
			var queue []*pendingLoginReq
			for _, m := range held {
				if fresh(m.StoredAt) {
					queue = append(queue, &pendingLoginReq{msg: m.Msg, slackChannel: m.SlackChannel, storedAt: m.StoredAt})
				}
			}
			if len(queue) == 0 {
				continue
			}
			if a.pendingLogin == nil {
				a.pendingLogin = make(map[string]map[string][]*pendingLoginReq)
			}
			if a.pendingLogin[user] == nil {
				a.pendingLogin[user] = make(map[string][]*pendingLoginReq)
			}
			a.pendingLogin[user][threadID] = lastN(append(queue, a.pendingLogin[user][threadID]...), maxParkedPerThread)
			linkUsers[user] = true
		}
		a.pendingLoginMu.Unlock()

		a.pendingAccessMu.Lock()
		for user, held := range h.Access {
			var queue []*pendingAccessReq
			for _, m := range held {
				if fresh(m.StoredAt) {
					queue = append(queue, &pendingAccessReq{msg: m.Msg, slackChannel: m.SlackChannel, storedAt: m.StoredAt})
				}
			}
			if len(queue) == 0 {
				continue
			}
			if a.pendingAccess == nil {
				a.pendingAccess = make(map[string]map[string][]*pendingAccessReq)
			}
			if a.pendingAccess[threadID] == nil {
				a.pendingAccess[threadID] = make(map[string][]*pendingAccessReq)
			}
			a.pendingAccess[threadID][user] = lastN(append(queue, a.pendingAccess[threadID][user]...), maxParkedPerThread)
		}
		a.pendingAccessMu.Unlock()

		if h.Pending != nil && fresh(h.Pending.StoredAt) {
			task := h.Pending.Task
			task.storedAt = h.Pending.StoredAt
			a.withThread(threadID, func(st *threadState) {
				if st.pending == nil {
					st.pending = &task
				}
			})
		}
	}

	for _, k := range threads {
		a.persistHeld(k.ChannelID, k.ThreadID)
	}
	if len(threads) > 0 {
		a.Logger.Info("slack: held thread state restored", "threads", len(threads))
	}

	if a.OBO == nil {
		return
	}
	for user := range linkUsers {
		if _, err := a.OBO.TokenFor(ctx, user); err == nil {
			a.OnUserLinked(ctx, user, "")
		}
	}
}

// lastN keeps the last n elements of s.
func lastN[T any](s []T, n int) []T {
	if len(s) > n {
		return s[len(s)-n:]
	}
	return s
}
