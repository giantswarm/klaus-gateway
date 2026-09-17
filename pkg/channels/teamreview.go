package channels

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
)

// TeamReview is an ask a manager posts into a team's channel: the change
// spelled out, a link for anything the button does not cover, and the tool
// call a member's approval makes. It has no initiator — the decision rule is
// the named team's: any linked member may decide, once.
type TeamReview struct {
	// Team is the GitHub team (slug) whose linked members may decide. The
	// gateway passes the click on as that member; the tool called checks the
	// member's membership against GitHub before it acts.
	Team string `json:"team"`
	// Channel is the Slack channel ID (C…) the ask is posted to.
	Channel string `json:"channel"`
	// Text spells the change out in Slack mrkdwn: what, which repository, the
	// giving and the receiving team for a transfer.
	Text string `json:"text"`
	// Link opens the PR (or whatever carries the change) for anything else.
	// Optional; http(s) only.
	Link string `json:"link,omitempty"`
	// Approve is the muster tool call the deciding member's click makes, under
	// that member's identity.
	Approve ToolInvocation `json:"approve"`
}

// TeamNotice is a message to a team's channel that asks for nothing: a
// completion, or the giving team's notice of a transfer. It renders without
// buttons.
type TeamNotice struct {
	Team    string `json:"team"`
	Channel string `json:"channel"`
	Text    string `json:"text"`
	Link    string `json:"link,omitempty"`
}

// ToolInvocation names a muster tool and the arguments to call it with, verbatim.
type ToolInvocation struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

// PostReceipt locates a posted message. ID is the gateway's handle on a
// review (empty for a notice).
type PostReceipt struct {
	ID      string `json:"id,omitempty"`
	Channel string `json:"channel"`
	TS      string `json:"ts"`
}

// TeamReviewPoster delivers team reviews and notices to a channel.
type TeamReviewPoster interface {
	PostTeamReview(ctx context.Context, review TeamReview) (PostReceipt, error)
	PostTeamNotice(ctx context.Context, notice TeamNotice) (PostReceipt, error)
}

// slackChannelID is the shape of a Slack conversation ID. chat.update needs
// the ID, not a name, so a name is refused up front instead of failing at the
// first click.
var slackChannelID = regexp.MustCompile(`^[CDG][A-Z0-9]{5,}$`)

// TeamTextMax bounds the text of a review or notice; it becomes one Slack
// section block, whose limit is 3000 characters.
const TeamTextMax = 3000

// Validate reports the first field that would make the review undeliverable.
func (r TeamReview) Validate() error {
	if err := validateTeamMessage(r.Team, r.Channel, r.Text, r.Link); err != nil {
		return err
	}
	if r.Approve.Tool == "" {
		return errors.New("approve.tool is required")
	}
	return nil
}

// Validate reports the first field that would make the notice undeliverable.
func (n TeamNotice) Validate() error {
	return validateTeamMessage(n.Team, n.Channel, n.Text, n.Link)
}

func validateTeamMessage(team, channel, text, link string) error {
	switch {
	case team == "":
		return errors.New("team is required")
	case !slackChannelID.MatchString(channel):
		return fmt.Errorf("channel %q is not a Slack channel ID", channel)
	case text == "":
		return errors.New("text is required")
	case len(text) > TeamTextMax:
		return fmt.Errorf("text exceeds %d characters", TeamTextMax)
	}
	if link == "" {
		return nil
	}
	u, err := url.Parse(link)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("link %q is not an http(s) URL", link)
	}
	return nil
}
