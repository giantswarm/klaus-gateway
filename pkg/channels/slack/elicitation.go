package slack

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/net/idna"

	"github.com/giantswarm/klaus-gateway/pkg/muster"
)

// ElicitationReturnPath is the gateway's generic return from a URL
// elicitation's page on muster's origin: muster sends the browser here after
// the person acted, with the elicitation's elicitationId, and the gateway
// resumes the prompt that showed it as its Done would. It shares the
// /auth/slack/ prefix the public route already sends to the gateway.
const ElicitationReturnPath = "/auth/slack/return"

// elicitationIDParam is the query parameter muster appends to the return.
const elicitationIDParam = "elicitationId"

// elicitationReturnPage is what the browser shows after the return. It never
// says what was decided: the outcome is muster's answer to the retried call,
// which the conversation shows.
const elicitationReturnPage = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><title>Back to Slack</title></head>
<body><p>You can close this tab and go back to Slack. If the conversation does not continue there, choose <b>Done</b> on its prompt.</p></body></html>
`

// shownHost is the host a link opens as the person is shown it: its punycode
// form for an internationalized domain, as written when it has none.
func shownHost(link string) (shown, host string) {
	if u, err := url.Parse(link); err == nil {
		host = u.Hostname()
	}
	shown, err := idna.Lookup.ToASCII(host)
	if err != nil {
		shown = host
	}
	return shown, host
}

// elicitationOpenButtonLabel labels a URL elicitation's link button: neutral,
// with the host it opens.
func elicitationOpenButtonLabel(link string) string {
	shown, _ := shownHost(link)
	if shown == "" {
		return elicitationOpenLabel
	}
	return truncateButtonLabel(elicitationOpenLabel + " " + shown)
}

// elicitationDoneButton is a URL elicitation prompt's Done button carrying the
// prompt's completion state.
func elicitationDoneButton(stateID string) map[string]any {
	return map[string]any{
		bkType:     bkButton,
		bkText:     map[string]any{bkType: bkPlainText, bkText: elicitationDoneLabel},
		bkActionID: elicitationDone,
		bkValue:    stateID,
	}
}

// elicitationLink is the link a runtime's URL elicitation prompt opens:
// the server's, with the gateway's return added when it is on muster's origin
// and the return is configured. A link the return would make unshowable is
// kept as the server gave it.
func (a *Adapter) elicitationLink(link, elicitationID string) string {
	withReturn := muster.ReturnLink(link, a.MusterURL, a.ElicitationReturnURL, elicitationID)
	if muster.ValidateSignInURL(withReturn) != nil {
		return link
	}
	return withReturn
}

// handleElicitationDone is a click on a URL elicitation prompt's Done: the
// person is done at its link. The first click of the person the prompt is
// for runs what the prompt holds; any other is a no-op.
func (a *Adapter) handleElicitationDone(slackUser, stateID, responseURL string) {
	entry, ok := a.takeConnectorAnswer(slackUser, stateID)
	if !ok {
		return
	}
	a.background(func(ctx context.Context) { a.continueAfterConsent(ctx, entry, responseURL) })
}

// takeElicitationReturn marks every live, unanswered completion state of
// elicitationID answered and returns them.
func (a *Adapter) takeElicitationReturn(elicitationID string) []connectorCompletion {
	if elicitationID == "" {
		return nil
	}
	now := time.Now()
	a.connectorCompletionsMu.Lock()
	defer a.connectorCompletionsMu.Unlock()
	var taken []connectorCompletion
	for id, stored := range a.connectorCompletions {
		if stored.value.elicitationID != elicitationID || stored.value.consented || now.After(stored.expires) {
			continue
		}
		stored.value.consented = true
		a.connectorCompletions[id] = stored
		taken = append(taken, stored.value)
	}
	return taken
}

// HandleElicitationReturn serves ElicitationReturnPath: muster's browser
// return after the person acted at a URL elicitation's link. Every prompt
// still waiting on that elicitationId continues as its Done would, as the
// person it was posted for; muster decides the outcome on the retried call,
// so the return carries no decision and the page states none. An unknown or
// missing id gets the same page.
func (a *Adapter) HandleElicitationReturn(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	for _, entry := range a.takeElicitationReturn(r.URL.Query().Get(elicitationIDParam)) {
		a.background(func(ctx context.Context) { a.continueAfterConsent(ctx, entry, "") })
	}
	h := w.Header()
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
	h.Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write([]byte(elicitationReturnPage))
}
