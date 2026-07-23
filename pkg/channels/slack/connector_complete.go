package slack

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/auth/musterlink"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

// ConnectorCompletePath is the browser-facing landing muster redirects to
// after a connector sign-in started from a Slack Connect button. The Connect
// prompt appends redirect=<PublicBaseURL><ConnectorCompletePath>?s=<state> to
// the muster login start URL; muster validates the redirect against its
// post-login allowlist and 303s the browser here on callback success.
const ConnectorCompletePath = "/connectors/complete"

// connectorCompletion binds a decorated Connect button to its Slack
// coordinates so the post-login landing can rewrite the ephemeral prompt and
// resume the conversation. The prompt is ephemeral, so the click's
// response_url is the only rewrite handle; it arrives at click time and the
// browser landing can race it in either order, hence whichever side arrives
// second performs the rewrite.
type connectorCompletion struct {
	slackUser       string // raw Slack user ID (U…)
	server          string // backend name; the trusted source for user-facing text
	channel         string // Slack channel the prompt was posted in
	threadTS        string // thread root; respondURL target and resume thread
	responseURL     string // recorded by the Connect click; empty until it arrives
	completed       bool   // landing arrived; the one-shot resume fired
	promptRewritten bool   // the ephemeral prompt was rewritten to the confirmation
}

// mintConnectorCompletion stores fresh completion state for a decorated
// Connect button and returns its opaque ID. The ID travels in the button value
// and in the redirect's s parameter; everything else stays server-side.
func (a *Adapter) mintConnectorCompletion(slackUser, server, channel, threadTS string) string {
	raw := make([]byte, 16)
	// crypto/rand.Read never returns an error (Go 1.24+).
	_, _ = rand.Read(raw)
	stateID := base64.RawURLEncoding.EncodeToString(raw)

	now := time.Now()
	a.connectorCompletionsMu.Lock()
	defer a.connectorCompletionsMu.Unlock()
	if a.connectorCompletions == nil {
		a.connectorCompletions = make(map[string]ttlEntry[connectorCompletion])
	}
	sweepExpired(a.connectorCompletions, now)
	a.connectorCompletions[stateID] = ttlEntry[connectorCompletion]{
		value: connectorCompletion{
			slackUser: slackUser,
			server:    server,
			channel:   channel,
			threadTS:  threadTS,
		},
		expires: now.Add(connectorCompletionTTL),
	}
	return stateID
}

// recordConnectorConnectClick stores a Connect click's response_url under
// stateID. ok is false for an unknown or expired stateID (including legacy
// buttons whose value is the server name) and for a clicker who is not the
// prompted user; the caller no-ops. rewriteNow is true when the browser
// landing already completed and the prompt is not yet rewritten: the click
// arrived second and owns the rewrite.
func (a *Adapter) recordConnectorConnectClick(slackUser, stateID, responseURL string) (entry connectorCompletion, rewriteNow, ok bool) {
	now := time.Now()
	a.connectorCompletionsMu.Lock()
	defer a.connectorCompletionsMu.Unlock()
	stored, found := a.connectorCompletions[stateID]
	if !found || now.After(stored.expires) || stored.value.slackUser != slackUser {
		return connectorCompletion{}, false, false
	}
	stored.value.responseURL = responseURL
	rewriteNow = stored.value.completed && !stored.value.promptRewritten && responseURL != ""
	if rewriteNow {
		stored.value.promptRewritten = true
	}
	a.connectorCompletions[stateID] = stored
	return stored.value, rewriteNow, true
}

// completeConnectorLanding marks stateID completed. resume is true only on the
// first landing, so a browser reload cannot re-dispatch the synthetic
// continuation. rewrite is true when a response_url is recorded and the prompt
// is not yet rewritten: the landing arrived second and owns the rewrite. ok is
// false for unknown or expired state. The entry is kept until its TTL sweep:
// a reload must keep rendering and a late click must still find the
// completion to rewrite the prompt.
func (a *Adapter) completeConnectorLanding(stateID string) (entry connectorCompletion, resume, rewrite, ok bool) {
	now := time.Now()
	a.connectorCompletionsMu.Lock()
	defer a.connectorCompletionsMu.Unlock()
	stored, found := a.connectorCompletions[stateID]
	if !found || now.After(stored.expires) {
		return connectorCompletion{}, false, false, false
	}
	resume = !stored.value.completed
	stored.value.completed = true
	rewrite = stored.value.responseURL != "" && !stored.value.promptRewritten
	if rewrite {
		stored.value.promptRewritten = true
	}
	a.connectorCompletions[stateID] = stored
	return stored.value, resume, rewrite, true
}

// decorateConnectorLoginURL appends
// redirect=<publicBaseURL><ConnectorCompletePath>?s=<stateID> to a
// muster-hosted login start URL, preserving its existing query parameters.
// muster validates the redirect against oauth.mcpClient.postLoginRedirectAllowlist
// and 303s the browser to it with server=<name> appended on callback success.
func decorateConnectorLoginURL(loginURL, publicBaseURL, stateID string) (string, error) {
	parsed, err := url.Parse(loginURL)
	if err != nil {
		return "", fmt.Errorf("parse login URL: %w", err)
	}
	if parsed.Scheme == "" || parsed.Host == "" {
		return "", errors.New("login URL is not absolute")
	}
	landing := strings.TrimRight(publicBaseURL, "/") + ConnectorCompletePath + "?s=" + url.QueryEscape(stateID)
	query := parsed.Query()
	query.Set("redirect", landing)
	parsed.RawQuery = query.Encode()
	return parsed.String(), nil
}

// handleConnectorComplete serves the post-login landing. It rewrites the
// ephemeral Connect prompt when the click's response_url has already been
// recorded (otherwise the click side does it), dispatches the one-shot
// synthetic continuation into the thread, and renders a "return to Slack"
// page. The server query parameter muster appends is never trusted for
// user-facing text; the state's stored server name is.
func (a *Adapter) handleConnectorComplete(w http.ResponseWriter, r *http.Request) {
	stateID := r.URL.Query().Get("s")
	entry, resume, rewrite, ok := a.completeConnectorLanding(stateID)
	if !ok {
		musterlink.RenderPage(w, http.StatusNotFound, musterlink.Page{
			Title:   "Sign-in confirmation expired",
			Heading: "Link expired",
			Message: "This confirmation link is invalid or has expired. Return to Slack and continue the conversation there.",
		})
		return
	}
	if reported := r.URL.Query().Get("server"); reported != "" && reported != entry.server {
		a.Logger.Warn("slack: connector completion server mismatch", "user", entry.slackUser, "stored", entry.server, "reported", reported)
	}

	if rewrite {
		responseURL, threadTS, server := entry.responseURL, entry.threadTS, entry.server
		a.background(func(ctx context.Context) {
			if err := respondURL(ctx, responseURL, threadTS, connectorSignedInNotice(server)); err != nil {
				a.Logger.Warn("slack: rewrite connector prompt after sign-in failed", "server", server, "error", err)
			}
		})
	}
	if resume {
		a.background(func(ctx context.Context) {
			a.resumeAfterConnectorSignIn(ctx, entry)
		})
	}

	musterlink.RenderPage(w, http.StatusOK, musterlink.Page{
		Title:   "Signed in to " + entry.server,
		Heading: "Signed in",
		Message: "You can close this tab and return to Slack; I'll pick the conversation back up there.",
	})
}

// resumeAfterConnectorSignIn dispatches the synthetic continuation into the
// thread that hit the auth challenge, so the agent retries the blocked tools
// without the user retyping. Blocking (a replayed turn runs to completion),
// so it runs on the adapter lifecycle context via background.
func (a *Adapter) resumeAfterConnectorSignIn(ctx context.Context, entry connectorCompletion) {
	msg := channels.InboundMessage{
		Channel:   ChannelName,
		ChannelID: entry.channel,
		ThreadID:  entry.threadTS,
		Text:      fmt.Sprintf(connectorResumeText, entry.server),
		Subject:   entry.slackUser,
	}
	if err := a.replayDispatch(ctx, msg, entry.channel); err != nil && !errors.Is(err, context.Canceled) {
		a.Logger.Error("slack: connector sign-in resume failed", "user", entry.slackUser, "thread", entry.threadTS, "error", err)
		a.postReplayFailureNote(ctx, entry.channel, entry.threadTS)
	}
}

// connectorSignedInNotice is the confirmation the ephemeral Connect prompt is
// rewritten to once the backend sign-in completes.
func connectorSignedInNotice(server string) string {
	return fmt.Sprintf("✅ _Signed in to %s._", escapeMrkdwn(server))
}
