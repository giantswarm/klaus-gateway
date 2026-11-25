package slack

import (
	"context"
	"crypto/subtle"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/net/idna"

	"github.com/giantswarm/klaus-gateway/pkg/muster"
)

// ElicitationReturnPath is the gateway's generic return from a URL
// elicitation's page on muster's origin: muster sends the browser here after
// the person acted, and the gateway resumes the prompt that showed the link
// as its Done would. It shares the /auth/slack/ prefix the public route
// already sends to the gateway.
const ElicitationReturnPath = "/auth/slack/return"

// elicitationIDParam is the query parameter muster appends to the return
// after an approval.
const elicitationIDParam = "elicitationId"

// returnPromptParam is the query parameter of the return the gateway puts on
// a prompt's link: the prompt's own random return key. muster's allowlist is
// a prefix match that leaves the query free, and muster keeps the query when
// it sends the browser back, so the return names the one prompt the gateway
// issued the link in, to its person alone, and nothing anyone else can
// derive from the elicitation.
const returnPromptParam = "prompt"

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
// with the host it opens. A host too long for a button keeps its right end,
// cut at a label and led by "…", so the registrable domain always shows; when
// not even its last two labels fit, the label names no host and the prompt's
// text, which shows the host and the whole link, is left to say it.
func elicitationOpenButtonLabel(link string) string {
	shown, _ := shownHost(link)
	if shown == "" {
		return elicitationOpenLabel
	}
	label := elicitationOpenLabel + " " + shown
	if utf8.RuneCountInString(label) <= slackButtonLabelMax {
		return label
	}
	labels := strings.Split(shown, ".")
	for i := 1; i <= len(labels)-2; i++ {
		label = elicitationOpenLabel + " …" + strings.Join(labels[i:], ".")
		if utf8.RuneCountInString(label) <= slackButtonLabelMax {
			return label
		}
	}
	return elicitationOpenLabel
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

// promptLink is the link a URL elicitation prompt opens, and the prompt's
// return key when the link carries the gateway's return: a link on muster's
// origin gets the configured return with a fresh key (muster.ReturnLink),
// and loses any redirect it brought. Any other link is opened as its server
// gave it and gets no key, so no return resumes its prompt. A link the return
// would make unshowable is kept without one.
func (a *Adapter) promptLink(link string) (shown, returnKey string) {
	if a.ElicitationReturnURL != "" {
		key := randomToken()
		withReturn := muster.ReturnLink(link, a.MusterURL, a.ElicitationReturnURL+"?"+url.Values{returnPromptParam: {key}}.Encode())
		if withReturn != link && muster.ValidateSignInURL(withReturn) == nil {
			return withReturn, key
		}
	}
	return muster.ReturnLink(link, a.MusterURL, ""), ""
}

// mintPrompt stores entry's completion state for a prompt opening link and
// returns the state's ID and the link the prompt shows (promptLink), whose
// return key entry keeps.
func (a *Adapter) mintPrompt(entry connectorCompletion, link string) (stateID, shown string) {
	shown, entry.returnKey = a.promptLink(link)
	return a.mintConnectorCompletion(entry), shown
}

// handleElicitationDone is a click on a URL elicitation prompt's Done: the
// person is done at its link. The first click of the person the prompt is
// for runs what the prompt holds; any other continues nothing and says why
// (tellDeadPrompt).
func (a *Adapter) handleElicitationDone(ctx context.Context, click promptClick) {
	entry, ok := a.takeConnectorAnswer(click.slackUser, click.stateID)
	if !ok {
		a.tellDeadPrompt(ctx, click)
		return
	}
	a.background(func(ctx context.Context) { a.continueAfterConsent(ctx, entry, click.continuationURL()) })
}

// takeElicitationReturn marks the live, unanswered completion state whose
// return key is returnKey answered and returns it. An elicitationID muster
// appended must be the prompt's own; a sign-in's return carries none.
func (a *Adapter) takeElicitationReturn(returnKey, elicitationID string) (connectorCompletion, bool) {
	if returnKey == "" {
		return connectorCompletion{}, false
	}
	now := time.Now()
	a.connectorCompletionsMu.Lock()
	defer a.connectorCompletionsMu.Unlock()
	for id, stored := range a.connectorCompletions {
		if stored.value.returnKey == "" || subtle.ConstantTimeCompare([]byte(stored.value.returnKey), []byte(returnKey)) != 1 {
			continue
		}
		if stored.value.consented || now.After(stored.expires) ||
			(elicitationID != "" && stored.value.elicitationID != "" && elicitationID != stored.value.elicitationID) {
			return connectorCompletion{}, false
		}
		stored.value.consented = true
		a.connectorCompletions[id] = stored
		return stored.value, true
	}
	return connectorCompletion{}, false
}

// HandleElicitationReturn serves ElicitationReturnPath: muster's browser
// return after the person acted at a URL elicitation's link. The prompt the
// return's key names, if it is still waiting, continues as its Done would, as
// the person it was posted for; muster decides the outcome on the retried
// call, so the return carries no decision and the page states none. An
// unknown, used or missing key gets the same page.
func (a *Adapter) HandleElicitationReturn(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, http.StatusText(http.StatusMethodNotAllowed), http.StatusMethodNotAllowed)
		return
	}
	query := r.URL.Query()
	if entry, ok := a.takeElicitationReturn(query.Get(returnPromptParam), query.Get(elicitationIDParam)); ok {
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
