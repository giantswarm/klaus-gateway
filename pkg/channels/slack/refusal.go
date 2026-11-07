package slack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// An egress refusal notice tells an agent's creator, by direct message from
// the app, that the egress gateway refused a destination the agent's egress
// does not declare: the destination, how many attempts in how many Sessions,
// and what they can do about it, worded as the Dev Portal's Session alert
// words it (giantswarm/giantswarm#38814). A coding agent reaches only the
// platform's list for its Harness, so only the platform team can widen it; an
// agent on the kagent runtime is allowed the destination on its edit page,
// which the notice's button opens; an agent whose Harness is not known gets
// no promise. Raw TCP is allowed to no agent, whatever its runtime: kagent's
// egress declares HTTP and HTTPS origins only.
// There is one notice per agent and destination within the window
// (RefusalWindow): refusals inside it are counted into the notice, which is
// rewritten in place, and the first refusal after it posts a new one. The
// record lives in the review store under an id derived from the agent and
// the destination, with the window as its TTL, so a gateway restart keeps
// the deduplication.

// DefaultRefusalWindow is how long one notice covers its agent and
// destination when RefusalWindow is unset.
const DefaultRefusalWindow = 24 * time.Hour

// refusalOpenAgent is the notice's URL button; the browser opens the link.
const refusalOpenAgent = "refusal_open_agent"

const (
	refusalTitle      = "Outgoing connection to %s blocked"
	refusalReason     = "%s is not allowed to reach this address"
	refusalReasonTCP  = "Raw TCP connections are not available to agents"
	refusalCountLine  = "%s in %s since %s, last at %s"
	refusalPlatform   = "%s can only reach the addresses the platform allows for %s. Ask your platform team to allow %s."
	refusalEdit       = "You can allow it: add `%s` to %s’s Extra egress origins."
	refusalTCP        = "No egress setting can allow it. If %s offers HTTPS, have %s use that instead."
	refusalNeutral    = "If %s needs %s, ask your platform team."
	refusalDetails    = "Technical details: destination `%s:%d (%s)`"
	refusalDetailsURL = refusalDetails + ", origin `%s`"
	refusalFooter     = "You get this because you created *%s*. Further refusals of this host are counted here; the next notice for it comes after %s."
	refusalEditLabel  = "Edit %s"
	refusalTimeLayout = "Jan 2 15:04 UTC"
)

// kagentRuntime is the Harness runtime of an agent that is no coding agent.
const kagentRuntime = "kagent"

// runtimeTitles name a Harness runtime as the Dev Portal does, for a
// Harness without a display name.
var runtimeTitles = map[string]string{
	"claude": "Claude Code",
	"codex":  "Codex",
	"byo":    "Custom image",
}

// refusalID is the record of the notice for one agent and destination.
func refusalID(r channels.EgressRefusal) string {
	sum := sha256.Sum256([]byte(r.Namespace + "/" + r.Agent + "|" + r.Scheme + "://" + r.Host + ":" + strconv.Itoa(r.Port)))
	return "refusal-" + hex.EncodeToString(sum[:16])
}

// NoteRefusal counts r into the notice for its agent and destination, or
// posts that notice to the agent's creator when none is live. It implements
// channels.RefusalNotifier; a creator who is not a member of the workspace is
// channels.ErrAddresseeNotFound.
func (a *Adapter) NoteRefusal(ctx context.Context, r channels.EgressRefusal) error {
	id := refusalID(r)
	now := time.Now()
	retention := max(a.refusalWindow(), DefaultRefusalWindow)
	var updated store.Review
	found, err := a.reviews().UpdateReview(ctx, id, func(rv *store.Review) bool {
		if rv.Refusal == nil {
			return false
		}
		next := *rv.Refusal
		previousCount := next.Count
		countRefusal(&next, r)
		increase := next.Count - previousCount
		if increase > 0 && r.At.Sub(rv.Refusal.FirstAt) >= a.refusalWindow() {
			next.Count, next.Sessions, next.MoreSessions = 0, nil, 0
			next.FirstAt, next.LastAt = time.Time{}, time.Time{}
			current := r
			current.Count, current.Counters = increase, nil
			countRefusal(&next, current)
			rv.Channel, rv.TS = "", ""
		}
		rv.Refusal = &next
		rv.TTL = now.Sub(rv.PostedAt) + retention
		updated = *rv
		return true
	})
	if err != nil {
		return fmt.Errorf("slack: look up refusal notice: %w", err)
	}
	if found && updated.Refusal != nil && updated.TS != "" {
		return a.apiClient().chatUpdate(ctx, updated.Channel, updated.TS, refusalFallback(*updated.Refusal), a.refusalBlocks(*updated.Refusal))
	}

	user, err := a.apiClient().lookupUserByEmail(ctx, r.Creator)
	if err != nil {
		return fmt.Errorf("slack: find %s: %w", r.Creator, err)
	}
	rec := store.Refusal{
		Namespace: r.Namespace, Agent: r.Agent, DisplayName: r.DisplayName, Creator: r.Creator,
		Runtime: r.Runtime, HarnessTitle: r.HarnessTitle,
		Scheme: r.Scheme, Host: r.Host, Port: r.Port, AgentURL: r.AgentURL,
	}
	if found && updated.Refusal != nil {
		rec = *updated.Refusal
	} else {
		countRefusal(&rec, r)
	}
	resp, err := a.apiClient().postJSONResponse(ctx, methodChatPostMessage, map[string]any{
		paramChannel: user,
		paramText:    refusalFallback(rec),
		paramBlocks:  a.refusalBlocks(rec),
	})
	if err != nil {
		return err
	}
	rv := store.Review{
		ID: id, Channel: resp.Channel, TS: resp.Ts,
		Text: refusalFallback(rec), Refusal: &rec,
		PostedAt: now, TTL: retention,
	}
	if rv.Channel == "" {
		rv.Channel = user
	}
	if err := a.reviews().PutReview(ctx, rv); err != nil {
		// The notice is out; without its record the next refusal posts a
		// second one, which is better than none.
		return fmt.Errorf("slack: record refusal notice: %w", err)
	}
	a.Logger.Info("slack: egress refusal notice posted", "record", "refusal_notice_posted",
		"agent", r.Namespace+"/"+r.Agent, "destination", r.Scheme+"://"+r.Host+":"+strconv.Itoa(r.Port), "creator", r.Creator, "count", r.Count)
	return nil
}

// countRefusal adds the requests and Sessions of r to rec.
func countRefusal(rec *store.Refusal, r channels.EgressRefusal) {
	increase := r.Count
	if len(r.Counters) > 0 {
		increase = 0
		baselineOnly := rec.Count > 0 && len(rec.Counters) == 0
		counters := maps.Clone(rec.Counters)
		if counters == nil {
			counters = make(map[string]store.RefusalCounter)
		}
		for key, counter := range counters {
			if r.At.Sub(counter.ObservedAt) > DefaultRefusalWindow {
				delete(counters, key)
			}
		}
		for _, counter := range r.Counters {
			delta := counter.Increase
			if previous, ok := counters[counter.Key]; ok {
				delta = counter.Value
				if counter.Value >= previous.Value {
					delta -= previous.Value
				}
			}
			if !baselineOnly {
				increase += int(delta)
			}
			counters[counter.Key] = store.RefusalCounter{Value: counter.Value, ObservedAt: r.At}
		}
		rec.Counters = counters
	}
	if increase <= 0 {
		return
	}
	rec.Count += increase
	if rec.FirstAt.IsZero() || r.At.Before(rec.FirstAt) {
		rec.FirstAt = r.At
	}
	if r.At.After(rec.LastAt) {
		rec.LastAt = r.At
	}
	if r.DisplayName != "" {
		rec.DisplayName = r.DisplayName
	}
	if r.Runtime != "" {
		rec.Runtime, rec.HarnessTitle = r.Runtime, r.HarnessTitle
	}
	for _, s := range r.Sessions {
		switch {
		case slices.Contains(rec.Sessions, s):
		case len(rec.Sessions) < store.RefusalSessionsMax:
			rec.Sessions = append(rec.Sessions, s)
		default:
			rec.MoreSessions++
		}
	}
}

func (a *Adapter) refusalWindow() time.Duration {
	if a.RefusalWindow > 0 {
		return a.RefusalWindow
	}
	return DefaultRefusalWindow
}

// refusalName is how the notice names the agent.
func refusalName(rec store.Refusal) string {
	if rec.DisplayName != "" {
		return rec.DisplayName
	}
	return rec.Agent
}

// refusalOrigin is the origin the agent's egress would declare to allow the
// destination: the scheme's default port is left out.
func refusalOrigin(rec store.Refusal) string {
	if defaultPorts[rec.Scheme] == rec.Port || rec.Port == 0 {
		return rec.Scheme + "://" + rec.Host
	}
	return rec.Scheme + "://" + rec.Host + ":" + strconv.Itoa(rec.Port)
}

// defaultPorts are the ports an origin of the scheme leaves out.
var defaultPorts = map[string]int{schemeHTTP: 80, schemeHTTPS: 443}

const (
	schemeHTTP  = "http"
	schemeHTTPS = schemeHTTP + "s"
	schemeTCP   = "tcp"
)

// refusalRuntime names the agent's Harness: its display name, else its
// runtime's title, else the runtime as written.
func refusalRuntime(rec store.Refusal) string {
	if rec.HarnessTitle != "" {
		return rec.HarnessTitle
	}
	if title, ok := runtimeTitles[rec.Runtime]; ok {
		return title
	}
	return rec.Runtime
}

// refusalRemedy is what the creator can do about the refusal, and whether
// the agent's edit page is where they do it.
func refusalRemedy(rec store.Refusal, name string) (string, bool) {
	switch {
	case rec.Scheme == schemeTCP:
		return fmt.Sprintf(refusalTCP, rec.Host, name), false
	case rec.Runtime == "":
		return fmt.Sprintf(refusalNeutral, name, rec.Host), false
	case rec.Runtime != kagentRuntime:
		return fmt.Sprintf(refusalPlatform, name, escapeMrkdwn(refusalRuntime(rec)), rec.Host), false
	default:
		return fmt.Sprintf(refusalEdit, refusalOrigin(rec), name), true
	}
}

func refusalFallback(rec store.Refusal) string {
	return truncateRunes(fmt.Sprintf(refusalTitle, rec.Host)+" ("+refusalName(rec)+")", slackSectionTextMax)
}

func (a *Adapter) refusalBlocks(rec store.Refusal) []any {
	name := escapeMrkdwn(refusalName(rec))
	reason := fmt.Sprintf(refusalReason, name)
	details := fmt.Sprintf(refusalDetailsURL, rec.Host, rec.Port, rec.Scheme, refusalOrigin(rec))
	if rec.Scheme == schemeTCP {
		reason = refusalReasonTCP
		details = fmt.Sprintf(refusalDetails, rec.Host, rec.Port, rec.Scheme)
	}
	sessions := len(rec.Sessions) + rec.MoreSessions
	count := fmt.Sprintf(refusalCountLine,
		countOf(int64(rec.Count), "attempt"), countOf(int64(sessions), "Session"),
		rec.FirstAt.UTC().Format(refusalTimeLayout), rec.LastAt.UTC().Format(refusalTimeLayout))
	remedy, edit := refusalRemedy(rec, name)
	blocks := []any{
		map[string]any{bkType: bkHeader, bkText: map[string]any{bkType: bkPlainText, bkText: truncateRunes(fmt.Sprintf(refusalTitle, rec.Host), 150)}},
		map[string]any{bkType: bkSection, bkText: verbatimMrkdwn(truncateRunes(reason+" · "+count+"\n"+remedy, slackSectionTextMax))},
	}
	if edit && rec.AgentURL != "" {
		blocks = append(blocks, map[string]any{
			bkType: bkActions,
			bkElements: []any{map[string]any{
				bkType:     bkButton,
				bkText:     map[string]any{bkType: bkPlainText, bkText: truncateRunes(fmt.Sprintf(refusalEditLabel, refusalName(rec)), 75)},
				bkActionID: refusalOpenAgent,
				bkURL:      rec.AgentURL,
			}},
		})
	}
	blocks = append(blocks, map[string]any{
		bkType: bkContext,
		bkElements: []any{
			verbatimMrkdwn(details),
			verbatimMrkdwn(fmt.Sprintf(refusalFooter, name, spellDuration(a.refusalWindow()))),
		},
	})
	return blocks
}

// verbatimMrkdwn is a mrkdwn text object Slack renders as written: a refused
// destination such as api.anthropic.com:443 stays text instead of becoming an
// http://…:443 link.
func verbatimMrkdwn(md string) map[string]any {
	return map[string]any{bkType: bkMrkdwn, bkText: md, bkVerbatim: true}
}

var _ channels.RefusalNotifier = (*Adapter)(nil)
