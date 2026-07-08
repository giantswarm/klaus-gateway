package slack

import (
	"context"
	"errors"
	"log/slog"
)

// progressState renders turn progress. In reactions mode it swaps an emoji on
// the triggering message (working → done/failed); in text mode (reactTS == "")
// the placeholder message and the streamed answer are the only signal, so the
// terminal hooks are no-ops.
type progressState struct {
	client  *slackAPIClient
	channel string
	reactTS string // triggering message ts; "" = text mode
	working bool   // a working reaction is currently present
	emojis  progressEmojis
	logger  *slog.Logger
}

type progressEmojis struct{ working, done, failed string }

func (p *progressState) done(ctx context.Context)   { p.swap(ctx, p.emojis.done) }
func (p *progressState) failed(ctx context.Context) { p.swap(ctx, p.emojis.failed) }

// clear removes the working reaction without adding a terminal one, used when
// the turn pauses waiting on the user.
func (p *progressState) clear(ctx context.Context) { p.removeWorking(ctx) }

func (p *progressState) swap(ctx context.Context, to string) {
	if p.reactTS == "" {
		return
	}
	p.removeWorking(ctx)
	if err := p.client.reactionsAdd(ctx, p.channel, p.reactTS, to); err != nil {
		p.logger.Warn("slack: add progress reaction failed", "emoji", to, "error", err)
	}
}

func (p *progressState) removeWorking(ctx context.Context) {
	if !p.working {
		return
	}
	p.working = false
	if err := p.client.reactionsRemove(ctx, p.channel, p.reactTS, p.emojis.working); err != nil {
		p.logger.Debug("slack: remove working reaction failed", "error", err)
	}
}

// startProgress begins progress rendering and returns the batched writer's seed
// ts ("" means post the reply lazily on the first flush). In reactions mode the
// working reaction is added to triggerTS; on a missing scope in auto mode it
// caches the downgrade and falls back to a text placeholder. triggerTS == ""
// (e.g. a button resume) always uses text mode.
func (a *Adapter) startProgress(ctx context.Context, client *slackAPIClient, channel, threadID, triggerTS, placeholder string) (*progressState, string) {
	p := &progressState{client: client, channel: channel, emojis: a.progressEmojis(), logger: a.Logger}

	if triggerTS != "" && a.reactionsMode() {
		switch err := client.reactionsAdd(ctx, channel, triggerTS, p.emojis.working); {
		case err == nil:
			p.reactTS = triggerTS
			p.working = true
			return p, "" // reactions mode: reply is posted lazily on first flush
		case errors.Is(err, errReactionsUnsupported):
			if a.ProgressMode == "" || a.ProgressMode == progressModeAuto {
				a.reactionsUnsupported.Store(true)
			}
			a.Logger.Warn("slack: reactions unavailable, using text progress", "error", err)
		default:
			a.Logger.Warn("slack: add working reaction failed", "error", err)
		}
	}

	// Text mode: post the placeholder; the streamed answer replaces it. If the
	// placeholder post fails, seed the writer lazily so the answer still lands.
	ts, err := client.postMessage(ctx, channel, placeholder, threadID)
	if err != nil {
		a.Logger.Warn("slack: post placeholder failed", "error", err)
		return p, ""
	}
	return p, ts
}

// reactionsMode reports whether this turn should attempt reaction-based
// progress, honoring the configured mode and the cached auto-mode downgrade.
func (a *Adapter) reactionsMode() bool {
	switch a.ProgressMode {
	case progressModeText:
		return false
	case progressModeReactions:
		return true
	default: // auto (also the zero value)
		return !a.reactionsUnsupported.Load()
	}
}

func (a *Adapter) progressEmojis() progressEmojis {
	e := progressEmojis{working: a.WorkingEmoji, done: a.DoneEmoji, failed: a.FailedEmoji}
	if e.working == "" {
		e.working = defaultWorkingEmoji
	}
	if e.done == "" {
		e.done = defaultDoneEmoji
	}
	if e.failed == "" {
		e.failed = defaultFailedEmoji
	}
	return e
}

// acquireThread reserves the single in-flight turn slot for threadID, returning
// false when a turn is already running (the caller rejects the new turn).
func (a *Adapter) acquireThread(threadID string) bool {
	a.inflightMu.Lock()
	defer a.inflightMu.Unlock()
	if a.inflight == nil {
		a.inflight = make(map[string]struct{})
	}
	if _, busy := a.inflight[threadID]; busy {
		return false
	}
	a.inflight[threadID] = struct{}{}
	return true
}

func (a *Adapter) releaseThread(threadID string) {
	a.inflightMu.Lock()
	delete(a.inflight, threadID)
	a.inflightMu.Unlock()
}
