package slack

import (
	"cmp"
	"context"
	"errors"
	"log/slog"
)

// progressState renders turn progress as an emoji on the triggering message
// (working → done/failed). A turn without a reaction (reactTS == "") has
// Slack's working indicator and the streamed answer as its only signal, so the
// terminal hooks are no-ops.
type progressState struct {
	client      *slackAPIClient
	channel     string
	reactTS     string // triggering message ts; "" = no reaction
	working     bool   // a working reaction is currently present
	clearOnDone bool   // on done, just remove the working reaction (no done reaction)
	emojis      progressEmojis
	logger      *slog.Logger
}

type progressEmojis struct{ working, done, failed string }

// done ends a successful turn. When clearOnDone is set it just removes the
// working reaction, leaving no residual emoji; otherwise it swaps in the done
// reaction.
func (p *progressState) done(ctx context.Context) {
	if p.clearOnDone {
		p.removeWorking(ctx)
		return
	}
	p.swap(ctx, p.emojis.done)
}

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
	if err := p.client.reactionsRemove(ctx, p.channel, p.reactTS, p.emojis.working); err != nil {
		// working stays true so a later terminal hook retries the removal
		// instead of stranding the reaction after a transient failure.
		p.logger.Debug("slack: remove working reaction failed", "error", err)
		return
	}
	p.working = false
}

// startProgress adds the working reaction to triggerTS. A turn with no
// triggering message (a button or form resume, a resume after sign-in) gets no
// reaction; neither does one after Slack refused reactions for lack of scope,
// which is cached so later turns skip the doomed call. No message is posted
// either way: Slack's working indicator shows that the turn runs, where the
// install accepts the agent session status.
func (a *Adapter) startProgress(ctx context.Context, client *slackAPIClient, channel, triggerTS string) *progressState {
	p := &progressState{client: client, channel: channel, clearOnDone: a.ClearReactionOnDone, emojis: a.progressEmojis(), logger: a.Logger}
	if triggerTS == "" || a.reactionsUnsupported.Load() {
		return p
	}
	switch err := client.reactionsAdd(ctx, channel, triggerTS, p.emojis.working); {
	case err == nil:
		p.reactTS = triggerTS
		p.working = true
	case errors.Is(err, errReactionsUnsupported):
		a.reactionsUnsupported.Store(true)
		a.Logger.Warn("slack: reactions unavailable, showing progress without them", "error", err)
	default:
		a.Logger.Warn("slack: add working reaction failed", "error", err)
	}
	return p
}

func (a *Adapter) progressEmojis() progressEmojis {
	return progressEmojis{
		working: cmp.Or(a.WorkingEmoji, defaultWorkingEmoji),
		done:    cmp.Or(a.DoneEmoji, defaultDoneEmoji),
		failed:  cmp.Or(a.FailedEmoji, defaultFailedEmoji),
	}
}
