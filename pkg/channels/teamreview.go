package channels

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

// TeamReview is an ask a manager posts into a team's channel: the change
// spelled out, the pull requests it lands as, a link for anything the buttons
// do not cover, and the tool calls a member's decision makes. It has no
// initiator — the decision rule is the named team's: any linked member may
// decide, once. Naming the actor makes it an action's approval: one person's
// change, decided by a second person.
type TeamReview struct {
	// Team is the GitHub team (slug) whose linked members may decide. The
	// gateway passes the click on as that member; the tool called checks the
	// member's membership against GitHub before it acts.
	Team string `json:"team"`
	// Channel is the Slack channel ID (C…) the ask is posted to.
	Channel string `json:"channel"`
	// Text spells the change out in Slack mrkdwn: what, which repository, the
	// giving and the receiving team for a transfer; for an action the actor,
	// the targets, the capability and the inputs that changed.
	Text string `json:"text"`
	// Link opens whatever carries the change — a pull request, a workflow
	// run, a repository — for anything else. Optional; http(s) only.
	Link string `json:"link,omitempty"`
	// Actor is the person whose action the review decides, as the gateway
	// knows linked people: the email of their identity (the one their Slack
	// account was linked to). Their own Approve is refused — a second person
	// decides; their Deny withdraws the action. Optional.
	Actor string `json:"actor,omitempty"`
	// PullRequests are the pull requests the change lands as, one http(s) URL
	// each, rendered as links. Optional; at most TeamPullRequestsMax.
	PullRequests []string `json:"pullRequests,omitempty"`
	// Approve is the muster tool call the deciding member's click makes, under
	// that member's identity.
	Approve ToolInvocation `json:"approve"`
	// Deny, when given, adds a Deny button: the member types a reason, and the
	// tool is called as that member with the arguments plus "reason".
	Deny ToolInvocation `json:"deny,omitzero"`
	// NoticeChannel, when given, is a second Slack channel ID that receives
	// the same text as a notice — no buttons — when the review is posted.
	NoticeChannel string `json:"noticeChannel,omitempty"`
}

// TeamNotice is a message to a team's channel that asks for nothing: a
// completion, or the giving team's notice of a transfer. It renders without
// buttons.
type TeamNotice struct {
	Team         string   `json:"team"`
	Channel      string   `json:"channel"`
	Text         string   `json:"text"`
	Link         string   `json:"link,omitempty"`
	PullRequests []string `json:"pullRequests,omitempty"`
}

// TeamReviewResult is the outcome of an action a manager posts into its
// review's thread as a follow-up — merged, rolled out, probes green or the
// failing probe — so one thread carries the whole action.
type TeamReviewResult struct {
	Text string `json:"text"`
	Link string `json:"link,omitempty"`
}

// ToolInvocation names a muster tool and the arguments to call it with, verbatim.
type ToolInvocation struct {
	Tool      string         `json:"tool"`
	Arguments map[string]any `json:"arguments,omitempty"`
}

// PostReceipt locates a posted message. ID is the gateway's handle on a
// review (empty for a notice); NoticeTS the notice a review posted to its
// second channel, when it named one.
type PostReceipt struct {
	ID       string `json:"id,omitempty"`
	Channel  string `json:"channel"`
	TS       string `json:"ts"`
	NoticeTS string `json:"notice_ts,omitempty"`
}

// ErrReviewNotFound is returned by PostTeamReviewResult for a review the
// gateway holds no record of: unknown, or past its TTL.
var ErrReviewNotFound = errors.New("review not found")

// TeamReviewPoster delivers team reviews, notices and an action's results to
// a channel.
type TeamReviewPoster interface {
	PostTeamReview(ctx context.Context, review TeamReview) (PostReceipt, error)
	PostTeamNotice(ctx context.Context, notice TeamNotice) (PostReceipt, error)
	// PostTeamReviewResult posts result into the thread of the review id;
	// ErrReviewNotFound when the gateway holds no record of it.
	PostTeamReviewResult(ctx context.Context, id string, result TeamReviewResult) (PostReceipt, error)
}

// slackChannelID is the shape of a Slack conversation ID. chat.update needs
// the ID, not a name, so a name is refused up front instead of failing at the
// first click.
var slackChannelID = regexp.MustCompile(`^[CDG][A-Z0-9]{5,}$`)

// TeamTextMax bounds the text of a review, notice or result; it becomes one
// Slack section block, whose limit is 3000 characters.
const TeamTextMax = 3000

// TeamPullRequestsMax bounds the pull requests of a review or notice; the list
// is one section block, and 25 links fit its 3000 characters.
const TeamPullRequestsMax = 25

// Validate reports the first field that would make the review undeliverable.
func (r TeamReview) Validate() error {
	if err := validateTeamMessage(r.Team, r.Channel, r.Text, r.Link, r.PullRequests); err != nil {
		return err
	}
	switch {
	case r.Approve.Tool == "":
		return errors.New("approve.tool is required")
	case r.Deny.Tool == "" && len(r.Deny.Arguments) > 0:
		return errors.New("deny.tool is required when deny is given")
	case r.Actor != "" && strings.TrimSpace(r.Actor) == "":
		return errors.New("actor must not be blank")
	case r.NoticeChannel != "" && !slackChannelID.MatchString(r.NoticeChannel):
		return fmt.Errorf("noticeChannel %q is not a Slack channel ID", r.NoticeChannel)
	case r.NoticeChannel == r.Channel:
		return errors.New("noticeChannel must differ from channel")
	}
	return nil
}

// Validate reports the first field that would make the notice undeliverable.
func (n TeamNotice) Validate() error {
	return validateTeamMessage(n.Team, n.Channel, n.Text, n.Link, n.PullRequests)
}

// Validate reports the first field that would make the result undeliverable.
func (r TeamReviewResult) Validate() error {
	switch {
	case r.Text == "":
		return errors.New("text is required")
	case len(r.Text) > TeamTextMax:
		return fmt.Errorf("text exceeds %d characters", TeamTextMax)
	}
	return validateLink("link", r.Link)
}

func validateTeamMessage(team, channel, text, link string, pullRequests []string) error {
	switch {
	case team == "":
		return errors.New("team is required")
	case !slackChannelID.MatchString(channel):
		return fmt.Errorf("channel %q is not a Slack channel ID", channel)
	case text == "":
		return errors.New("text is required")
	case len(text) > TeamTextMax:
		return fmt.Errorf("text exceeds %d characters", TeamTextMax)
	case len(pullRequests) > TeamPullRequestsMax:
		return fmt.Errorf("pullRequests exceeds %d entries", TeamPullRequestsMax)
	}
	if err := validateLink("link", link); err != nil {
		return err
	}
	for _, pr := range pullRequests {
		if pr == "" {
			return errors.New("pullRequests must not contain an empty entry")
		}
		if err := validateLink("pullRequests entry", pr); err != nil {
			return err
		}
	}
	return nil
}

// validateLink accepts an empty link or an http(s) URL with a host.
func validateLink(field, link string) error {
	if link == "" {
		return nil
	}
	u, err := url.Parse(link)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return fmt.Errorf("%s %q is not an http(s) URL", field, link)
	}
	return nil
}
