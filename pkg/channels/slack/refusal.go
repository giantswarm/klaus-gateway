package slack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// An egress refusal notice tells an agent's creator, by direct message from
// the app, that the egress gateway refused a destination the agent's egress
// does not declare: the destination, why, how many requests in how many
// Sessions, and a button that opens the agent where its egress is edited.
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
	refusalTitle        = "Egress refused for %s"
	refusalReasonOrigin = "%s is not one of the hosts *%s* may reach. Declare `%s` in the agent's egress to allow it."
	refusalReasonTCP    = "*%s* opened a raw TCP connection to %s; agents reach hosts over HTTPS only."
	refusalCountLine    = "%s in %s since %s, last at %s."
	refusalFooter       = "You get this because you created *%s*. Further refusals of this host are counted here; the next notice for it comes after %s."
	refusalOpenLabel    = "Open agent"
	refusalTimeLayout   = "Jan 2 15:04 UTC"
)

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
	var updated store.Review
	found, err := a.reviews().UpdateReview(ctx, id, func(rv *store.Review) bool {
		if rv.Refusal == nil {
			return false
		}
		next := *rv.Refusal
		countRefusal(&next, r)
		rv.Refusal = &next
		updated = *rv
		return true
	})
	if err != nil {
		return fmt.Errorf("slack: look up refusal notice: %w", err)
	}
	if found && updated.Refusal != nil {
		return a.apiClient().chatUpdate(ctx, updated.Channel, updated.TS, refusalFallback(*updated.Refusal), a.refusalBlocks(*updated.Refusal))
	}

	user, err := a.apiClient().lookupUserByEmail(ctx, r.Creator)
	if err != nil {
		return fmt.Errorf("slack: find %s: %w", r.Creator, err)
	}
	rec := store.Refusal{
		Namespace: r.Namespace, Agent: r.Agent, DisplayName: r.DisplayName, Creator: r.Creator,
		Scheme: r.Scheme, Host: r.Host, Port: r.Port, AgentURL: r.AgentURL,
	}
	countRefusal(&rec, r)
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
		PostedAt: time.Now(), TTL: a.refusalWindow(),
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
	rec.Count += r.Count
	if rec.FirstAt.IsZero() || r.At.Before(rec.FirstAt) {
		rec.FirstAt = r.At
	}
	if r.At.After(rec.LastAt) {
		rec.LastAt = r.At
	}
	if r.DisplayName != "" {
		rec.DisplayName = r.DisplayName
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
var defaultPorts = map[string]int{"http": 80, "https": 443}

// refusalDestination is the destination as the notice names it.
func refusalDestination(rec store.Refusal) string {
	if rec.Port == 0 {
		return "`" + rec.Host + "`"
	}
	return "`" + rec.Host + ":" + strconv.Itoa(rec.Port) + "`"
}

func refusalFallback(rec store.Refusal) string {
	return truncateRunes(fmt.Sprintf(refusalTitle, refusalName(rec))+": "+rec.Host, slackSectionTextMax)
}

func (a *Adapter) refusalBlocks(rec store.Refusal) []any {
	name := escapeMrkdwn(refusalName(rec))
	reason := fmt.Sprintf(refusalReasonOrigin, refusalDestination(rec), name, refusalOrigin(rec))
	if rec.Scheme == "tcp" {
		reason = fmt.Sprintf(refusalReasonTCP, name, refusalDestination(rec))
	}
	sessions := len(rec.Sessions) + rec.MoreSessions
	count := fmt.Sprintf(refusalCountLine,
		countOf(int64(rec.Count), "request"), countOf(int64(sessions), "Session"),
		rec.FirstAt.UTC().Format(refusalTimeLayout), rec.LastAt.UTC().Format(refusalTimeLayout))
	blocks := []any{
		map[string]any{bkType: bkHeader, bkText: map[string]any{bkType: bkPlainText, bkText: truncateRunes(fmt.Sprintf(refusalTitle, refusalName(rec)), 150)}},
		map[string]any{bkType: bkSection, bkText: map[string]any{bkType: bkMrkdwn, bkText: truncateRunes(reason+"\n"+count, slackSectionTextMax)}},
	}
	if rec.AgentURL != "" {
		blocks = append(blocks, map[string]any{
			bkType: bkActions,
			bkElements: []any{map[string]any{
				bkType:     bkButton,
				bkText:     map[string]any{bkType: bkPlainText, bkText: refusalOpenLabel},
				bkActionID: refusalOpenAgent,
				bkURL:      rec.AgentURL,
			}},
		})
	}
	blocks = append(blocks, contextBlock(fmt.Sprintf(refusalFooter, name, spellDuration(a.refusalWindow()))))
	return blocks
}

var _ channels.RefusalNotifier = (*Adapter)(nil)
