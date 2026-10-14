package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// The authorize modal lists every muster server the person reaches with a
// grant of their own, its state and the link that starts the grant, so a
// person signs in once and grants everything in one place instead of meeting
// one Connect prompt per server after a failed turn. muster answers it in one
// call (core_auth_status with auth_urls); each link lands on the gateway's
// connector landing, which redraws the modal, so a row turns Connected as its
// grant lands.

// Authorize modal Block Kit IDs. authorizeAllOpen is the button that opens
// the modal (its value: the completion state of the Connect prompt it sits
// on, or empty); authorizeGrantOpen is a row's URL button, whose click Slack
// still reports and the gateway acks without action; authorizeRefresh redraws
// the modal with fresh links.
const (
	authorizeAllOpen       = "authorize_all_open"
	authorizeGrantOpen     = "authorize_grant_open"
	authorizeRefresh       = "authorize_refresh"
	authorizeModalCallback = "authorize_modal"
)

// musterAuthStatusTool is muster's read of the caller's state on every
// server; with auth_urls it issues the sign-in link of each missing grant.
const musterAuthStatusTool = "core_auth_status"

// authorizeModalTTL bounds how long an open modal is redrawn by landings: a
// grant link muster issues lives ten minutes, and Refresh issues new ones.
const authorizeModalTTL = time.Hour

// authorizeModalRowsMax bounds the rows of one modal: Slack takes 100 blocks
// in a view, and the header, the summary, the footer and Refresh take five.
const authorizeModalRowsMax = 90

// authorizeStatusBudget bounds one read of the person's grant status.
const authorizeStatusBudget = 20 * time.Second

// authorizeModal is one open authorize modal: whose it is and, when it was
// opened from a Connect prompt, that prompt's completion state, which the
// modal's link for the prompt's server carries so that granting it from the
// modal resumes what the prompt held.
type authorizeModal struct {
	slackUser   string
	originState string
	// mu serialises the modal's redraws, so a late read never overwrites a
	// newer one.
	mu sync.Mutex
}

// grantServer is one server of core_auth_status as the modal shows it.
type grantServer struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Kind       string `json:"kind"`
	GrantScope string `json:"grant_scope"`
	AuthURL    string `json:"auth_url"`
	Error      string `json:"error"`
	Suspended  bool   `json:"suspended"`
}

const (
	grantKindGrant        = "grant"
	grantKindSSO          = "sso"
	grantStatusConnected  = "connected"
	grantStatusAuthNeeded = "auth_required"
	grantStatusReauth     = "reauth_required"
)

// needsGrant reports whether the person still has to grant the server.
func (s grantServer) needsGrant() bool {
	return s.Kind == grantKindGrant && !s.Suspended && (s.Status == grantStatusAuthNeeded || s.Status == grantStatusReauth)
}

// authorizeModalOffered reports whether the gateway can show the modal: it
// reads the grant status as the linked person and lands their grants.
func (a *Adapter) authorizeModalOffered() bool {
	return a.Grants != nil && a.OBO != nil && a.ConnectorPrompts && !a.grantsUnsupported.Load()
}

// readGrantStatus asks muster, as slackUser, for their state on every server;
// withLinks issues the link of every missing grant.
func (a *Adapter) readGrantStatus(ctx context.Context, slackUser string, withLinks bool) ([]grantServer, error) {
	ctx, cancel := context.WithTimeout(ctx, authorizeStatusBudget)
	defer cancel()
	token, err := a.OBO.TokenFor(ctx, slackUser)
	if err != nil {
		return nil, fmt.Errorf("the person's token: %w", err)
	}
	res, err := a.Grants.CallTool(ctx, token, musterAuthStatusTool, map[string]any{"auth_urls": withLinks})
	if err != nil {
		return nil, err
	}
	if !res.Dispatched {
		// call_tool refused the tool itself: a muster older than
		// core_auth_status. The offer is withdrawn until a restart, so no
		// Connect prompt carries a button that cannot work.
		if a.grantsUnsupported.CompareAndSwap(false, true) {
			a.Logger.Warn("slack: muster has no "+musterAuthStatusTool+", the authorize modal is not offered", "answer", res.Text)
		}
		return nil, fmt.Errorf("%s unavailable: %s", musterAuthStatusTool, res.Text)
	}
	if res.IsError {
		return nil, fmt.Errorf("%s refused: %s", musterAuthStatusTool, res.Text)
	}
	raw := []byte(res.Structured)
	if len(raw) == 0 {
		raw = []byte(res.Text)
	}
	var status struct {
		Servers []grantServer `json:"servers"`
	}
	if err := json.Unmarshal(raw, &status); err != nil {
		return nil, fmt.Errorf("decode %s: %w", musterAuthStatusTool, err)
	}
	return status.Servers, nil
}

// openAuthorizeModal opens the modal for slackUser on triggerID at once, as a
// loading view (the trigger expires three seconds after the click, muster's
// answer can take longer), and fills it with views.update.
func (a *Adapter) openAuthorizeModal(ctx context.Context, slackUser, triggerID, originState string) {
	if !a.authorizeModalOffered() || triggerID == "" {
		return
	}
	viewID, err := a.apiClient().viewsOpenID(ctx, triggerID, authorizeModalView(authorizeLoadingBlocks(), false))
	if err != nil || viewID == "" {
		a.Logger.Warn("slack: open authorize modal failed", "user", slackUser, "error", err)
		return
	}
	a.authorizeModalsMu.Lock()
	if a.authorizeModals == nil {
		a.authorizeModals = make(map[string]ttlEntry[*authorizeModal])
	}
	now := time.Now()
	sweepExpired(a.authorizeModals, now)
	a.authorizeModals[viewID] = ttlEntry[*authorizeModal]{value: &authorizeModal{slackUser: slackUser, originState: originState}, expires: now.Add(authorizeModalTTL)}
	a.authorizeModalsMu.Unlock()
	a.background(func(ctx context.Context) { a.refreshAuthorizeModal(ctx, viewID) })
}

// authorizeModalFor is the open modal viewID, nil when it is unknown or
// expired.
func (a *Adapter) authorizeModalFor(viewID string) *authorizeModal {
	a.authorizeModalsMu.Lock()
	defer a.authorizeModalsMu.Unlock()
	stored, ok := a.authorizeModals[viewID]
	if !ok || time.Now().After(stored.expires) {
		return nil
	}
	return stored.value
}

// refreshAuthorizeModal redraws the modal viewID from a fresh read of its
// person's grant status, with a new link for every grant still missing.
func (a *Adapter) refreshAuthorizeModal(ctx context.Context, viewID string) {
	modal := a.authorizeModalFor(viewID)
	if modal == nil {
		return
	}
	modal.mu.Lock()
	defer modal.mu.Unlock()

	servers, err := a.readGrantStatus(ctx, modal.slackUser, true)
	var blocks []any
	if err != nil {
		a.Logger.Warn("slack: read grant status for the authorize modal failed", "user", modal.slackUser, "error", err)
		blocks = authorizeFailedBlocks()
	} else {
		blocks = authorizeModalBlocks(servers, func(s grantServer) string { return a.grantLink(modal, viewID, s) })
	}
	if err := a.apiClient().viewsUpdate(ctx, viewID, "", authorizeModalView(blocks, true)); err != nil {
		a.Logger.Warn("slack: update authorize modal failed", "user", modal.slackUser, "error", err)
	}
}

// grantLink is the link a row's Authorize button opens: muster's, carrying
// the gateway's landing when the gateway has a public base URL, so the
// landing redraws the modal. The row for the server of the prompt the modal
// was opened from lands on that prompt's own completion, which resumes what
// the prompt held.
func (a *Adapter) grantLink(modal *authorizeModal, viewID string, s grantServer) string {
	if s.AuthURL == "" || a.PublicBaseURL == "" {
		return s.AuthURL
	}
	stateID := ""
	if modal.originState != "" {
		if origin, ok := a.attachAuthorizeModal(modal.originState, modal.slackUser, viewID); ok && origin.server == s.Name {
			stateID = modal.originState
		}
	}
	if stateID == "" {
		stateID = a.mintConnectorCompletion(connectorCompletion{slackUser: modal.slackUser, server: s.Name, modalView: viewID})
	}
	decorated, err := decorateConnectorLoginURL(s.AuthURL, a.PublicBaseURL, stateID)
	if err != nil {
		a.Logger.Warn("slack: decorate grant link failed, the plain link is offered", "server", s.Name, "error", err)
		return s.AuthURL
	}
	return decorated
}

// offerAuthorizeAfterSignIn tells a person who just signed in which servers
// still wait for their grant, with the button that opens the modal; nothing
// when none does. The message goes to the person's direct conversation with
// the app.
func (a *Adapter) offerAuthorizeAfterSignIn(ctx context.Context, slackUser string) {
	if !a.authorizeModalOffered() {
		return
	}
	servers, err := a.readGrantStatus(ctx, slackUser, false)
	if err != nil {
		a.Logger.Info("slack: no authorize offer after sign-in, the grant status is unreadable", "user", slackUser, "error", err)
		return
	}
	waiting := 0
	for _, s := range servers {
		if s.needsGrant() {
			waiting++
		}
	}
	if waiting == 0 {
		return
	}
	text := authorizeOfferText(waiting)
	blocks := []any{
		map[string]any{bkType: bkSection, bkText: map[string]any{bkType: bkMrkdwn, bkText: text}},
		map[string]any{bkType: bkActions, bkElements: []any{authorizeAllButton("")}},
	}
	if _, err := a.apiClient().postBlocks(ctx, slackUser, "", text, blocks); err != nil {
		a.Logger.Warn("slack: post authorize offer after sign-in failed", "user", slackUser, "error", err)
	}
}

// authorizeOfferText is the after-sign-in note for waiting servers.
func authorizeOfferText(waiting int) string {
	tools := "1 tool needs"
	if waiting > 1 {
		tools = fmt.Sprintf("%d tools need", waiting)
	}
	return fmt.Sprintf("You're signed in. %s your one-time authorization before the agent can use them for you. Authorize them now so nothing stops a conversation later.", tools)
}

// authorizeAllButton opens the authorize modal; originState is the
// completion state of the Connect prompt it sits on, or empty.
func authorizeAllButton(originState string) map[string]any {
	return map[string]any{
		bkType:     bkButton,
		bkText:     map[string]any{bkType: bkPlainText, bkText: "Authorize all"},
		bkActionID: authorizeAllOpen,
		bkValue:    originState,
	}
}

// authorizeModalView wraps blocks into the modal. Only a filled modal has
// Refresh, which a loading one could race.
func authorizeModalView(blocks []any, refresh bool) map[string]any {
	if refresh {
		blocks = append(blocks, map[string]any{bkType: bkActions, bkElements: []any{map[string]any{
			bkType:     bkButton,
			bkText:     map[string]any{bkType: bkPlainText, bkText: "Refresh"},
			bkActionID: authorizeRefresh,
		}}})
	}
	return map[string]any{
		bkType:       bkModal,
		bkCallbackID: authorizeModalCallback,
		bkTitle:      map[string]any{bkType: bkPlainText, bkText: "Authorize your tools"},
		bkClose:      map[string]any{bkType: bkPlainText, bkText: "Done"},
		bkBlocks:     blocks,
	}
}

func authorizeLoadingBlocks() []any {
	return []any{map[string]any{bkType: bkSection, bkText: map[string]any{bkType: bkMrkdwn, bkText: "Reading which tools still need your authorization…"}}}
}

func authorizeFailedBlocks() []any {
	return []any{map[string]any{bkType: bkSection, bkText: map[string]any{bkType: bkMrkdwn,
		bkText: "Your tools could not be read right now. Try *Refresh* in a moment; the agent's Connect prompts keep working meanwhile."}}}
}

// authorizeModalBlocks are the filled modal's blocks: a summary, one row per
// server the person reaches with a grant (its state, and an Authorize button
// with link(s) while the grant is missing), and a line for the servers that
// come with the sign-in.
func authorizeModalBlocks(servers []grantServer, link func(grantServer) string) []any {
	var grants []grantServer
	viaSignIn := 0
	for _, s := range servers {
		switch s.Kind {
		case grantKindGrant:
			grants = append(grants, s)
		case grantKindSSO:
			viaSignIn++
		}
	}
	connected := 0
	for _, s := range grants {
		if s.Status == grantStatusConnected {
			connected++
		}
	}

	summary := "Every tool here is authorized. The agent can use them for you."
	if connected < len(grants) {
		summary = fmt.Sprintf("*%d of %d* tools are authorized. Authorize each one once; the agent can use it for you from then on, in every conversation.", connected, len(grants))
	}
	if len(grants) == 0 {
		summary = "No tool needs an authorization of its own."
	}
	blocks := []any{map[string]any{bkType: bkSection, bkText: map[string]any{bkType: bkMrkdwn, bkText: summary}}}
	if len(grants) > 0 {
		blocks = append(blocks, map[string]any{bkType: bkDivider})
	}

	shown := grants[:min(len(grants), authorizeModalRowsMax)]
	for _, s := range shown {
		blocks = append(blocks, authorizeRow(s, link))
	}
	var footer []string
	if hidden := len(grants) - len(shown); hidden > 0 {
		footer = append(footer, fmt.Sprintf("%d more not shown; authorize these first and Refresh.", hidden))
	}
	if viaSignIn > 0 {
		footer = append(footer, fmt.Sprintf("%d more come with your sign-in and need nothing else.", viaSignIn))
	}
	if len(footer) > 0 {
		blocks = append(blocks, contextBlock(strings.Join(footer, " ")))
	}
	return blocks
}

// authorizeRow is one server's row.
func authorizeRow(s grantServer, link func(grantServer) string) map[string]any {
	state := ":large_green_circle: Authorized"
	switch {
	case s.Suspended:
		state = ":white_circle: Deactivated by its administrator"
	case s.needsGrant() && s.AuthURL == "" && s.Error != "":
		state = ":warning: " + escapeMrkdwnCapped(s.Error, 200)
	case s.Status == grantStatusReauth:
		state = ":large_orange_circle: Authorize again"
	case s.needsGrant():
		state = ":large_orange_circle: Needs your authorization"
	case s.Status != grantStatusConnected:
		state = ":white_circle: Not reachable right now"
	}
	row := map[string]any{
		bkType: bkSection,
		bkText: map[string]any{bkType: bkMrkdwn, bkText: fmt.Sprintf("*%s*\n%s", escapeMrkdwn(s.Name), state)},
	}
	if !s.needsGrant() {
		return row
	}
	if url := link(s); url != "" {
		row[bkAccessory] = map[string]any{
			bkType:     bkButton,
			bkText:     map[string]any{bkType: bkPlainText, bkText: "Authorize"},
			bkStyle:    bkPrimary,
			bkActionID: authorizeGrantOpen,
			bkValue:    s.Name,
			bkURL:      url,
		}
	}
	return row
}

// errAuthorizeModalUnknown is a Refresh on a modal the gateway no longer
// knows (its process restarted, or it expired).
var errAuthorizeModalUnknown = errors.New("authorize modal unknown")

// handleAuthorizeRefresh redraws the modal a Refresh click came from. A modal
// the gateway no longer knows is adopted for its clicker, since the view is
// theirs.
func (a *Adapter) handleAuthorizeRefresh(ctx context.Context, slackUser, viewID string) error {
	if !a.authorizeModalOffered() || viewID == "" {
		return nil
	}
	if a.authorizeModalFor(viewID) == nil {
		now := time.Now()
		a.authorizeModalsMu.Lock()
		if a.authorizeModals == nil {
			a.authorizeModals = make(map[string]ttlEntry[*authorizeModal])
		}
		a.authorizeModals[viewID] = ttlEntry[*authorizeModal]{value: &authorizeModal{slackUser: slackUser}, expires: now.Add(authorizeModalTTL)}
		a.authorizeModalsMu.Unlock()
	} else if modal := a.authorizeModalFor(viewID); modal.slackUser != slackUser {
		return errAuthorizeModalUnknown
	}
	a.refreshAuthorizeModal(ctx, viewID)
	return nil
}
