package channels

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// Decision is a question a service puts to a person or a team through
// POST /decisions: one Slack message with the status quo, a Choose button per
// option, an "Answer in my own words" button, the due time and the default.
// The answer — a click, the modal or a reply in the message's thread — calls
// Answer.Tool through muster as the person who answered, with Answer.Arguments
// plus "choice" (the option, 1-based; absent for none) and "text" (their own
// words; absent for none). The tool decides whether the person may answer; the
// gateway only delivers.
type Decision struct {
	// Person is the email of the person the decision is for: a direct message
	// from the app. Exactly one of Person and Team is set.
	Person string `json:"person,omitempty"`
	// Team names the team the decision is for (shown in the message) and
	// Channel the Slack channel ID it is posted to; any member may answer.
	Team    string `json:"team,omitempty"`
	Channel string `json:"channel,omitempty"`
	// Note names the asker's note the decision is, shown in the context line.
	Note string `json:"note,omitempty"`
	// Question is the message's header: one line.
	Question string `json:"question"`
	// StatusQuo is what is true now and why the question arises, in Slack
	// mrkdwn.
	StatusQuo string                 `json:"statusQuo"`
	Options   []store.DecisionOption `json:"options,omitempty"`
	// Recommend is the recommended option, 1-based; 0 recommends none.
	Recommend int `json:"recommend,omitempty"`
	// Due is when the asker applies Default if nobody answered.
	Due     time.Time `json:"due"`
	Default string    `json:"default"`
	// AskedBy names who asks, shown in the context line.
	AskedBy string         `json:"askedBy"`
	Answer  ToolInvocation `json:"answer"`
}

// DecisionClose closes a decision's message: answered (by a click or
// elsewhere), defaulted at its due time, or withdrawn. Text, when given, is
// shown with the outcome.
type DecisionClose struct {
	Outcome string `json:"outcome"`
	Text    string `json:"text,omitempty"`
}

// ErrDecisionNotFound is returned by CloseDecision for a decision the gateway
// holds no record of: unknown, or past its due time plus seven days.
var ErrDecisionNotFound = errors.New("decision not found")

// ErrAddresseeNotFound is returned by PostDecision when the person's email
// names nobody in the Slack workspace.
var ErrAddresseeNotFound = errors.New("no Slack user has this email")

// DecisionPoster delivers decisions and closes them.
type DecisionPoster interface {
	PostDecision(ctx context.Context, decision Decision) (PostReceipt, error)
	// CloseDecision rewrites the decision's message to its outcome;
	// ErrDecisionNotFound when the gateway holds no record of it.
	CloseDecision(ctx context.Context, id string, closing DecisionClose) (PostReceipt, error)
}

// Limits of a decision, from the Slack blocks it renders to: the question is
// a header, a label a button's neighbour in a section and an option of a
// select, the status quo a section.
const (
	DecisionQuestionMax    = 150
	DecisionStatusQuoMax   = 3000
	DecisionOptionsMax     = 10
	DecisionLabelMax       = 75
	DecisionConsequenceMax = 2800
	DecisionDefaultMax     = 1000
	DecisionAskedByMax     = 200
	DecisionNoteMax        = 100
	DecisionAnswerMax      = 3000
)

// Validate reports the first field that would make the decision
// undeliverable. now is the time the due time must lie after.
func (d Decision) Validate(now time.Time) error {
	switch {
	case (d.Person == "") == (d.Team == ""):
		return errors.New("exactly one of person and team is required")
	case d.Person != "" && !strings.Contains(d.Person, "@"):
		return fmt.Errorf("person %q is not an email", d.Person)
	case d.Person != "" && d.Channel != "":
		return errors.New("channel is for a team; a person gets a direct message")
	case d.Team != "" && !slackChannelID.MatchString(d.Channel):
		return fmt.Errorf("channel %q is not a Slack channel ID", d.Channel)
	}
	if err := requiredLine("question", d.Question, DecisionQuestionMax); err != nil {
		return err
	}
	if err := required("statusQuo", d.StatusQuo, DecisionStatusQuoMax); err != nil {
		return err
	}
	if len(d.Options) > DecisionOptionsMax {
		return fmt.Errorf("options exceeds %d entries", DecisionOptionsMax)
	}
	for i, o := range d.Options {
		if err := requiredLine(fmt.Sprintf("options[%d].label", i), o.Label, DecisionLabelMax); err != nil {
			return err
		}
		if utf8.RuneCountInString(o.Consequence) > DecisionConsequenceMax {
			return fmt.Errorf("options[%d].consequence exceeds %d characters", i, DecisionConsequenceMax)
		}
	}
	switch {
	case d.Recommend < 0 || d.Recommend > len(d.Options):
		return fmt.Errorf("recommend %d names no option", d.Recommend)
	case d.Due.IsZero():
		return errors.New("due is required")
	case !d.Due.After(now):
		return errors.New("due lies in the past")
	case d.Answer.Tool == "":
		return errors.New("answer.tool is required")
	case utf8.RuneCountInString(d.Note) > DecisionNoteMax:
		return fmt.Errorf("note exceeds %d characters", DecisionNoteMax)
	}
	if err := required("default", d.Default, DecisionDefaultMax); err != nil {
		return err
	}
	return requiredLine("askedBy", d.AskedBy, DecisionAskedByMax)
}

// Validate reports what makes the close unusable.
func (c DecisionClose) Validate() error {
	switch c.Outcome {
	case store.DecisionAnswered, store.DecisionDefaulted, store.DecisionWithdrawn:
	default:
		return fmt.Errorf("outcome %q is not one of answered, defaulted, withdrawn", c.Outcome)
	}
	if utf8.RuneCountInString(c.Text) > DecisionAnswerMax {
		return fmt.Errorf("text exceeds %d characters", DecisionAnswerMax)
	}
	return nil
}

func required(field, value string, limit int) error {
	switch {
	case strings.TrimSpace(value) == "":
		return fmt.Errorf("%s is required", field)
	case utf8.RuneCountInString(value) > limit:
		return fmt.Errorf("%s exceeds %d characters", field, limit)
	}
	return nil
}

func requiredLine(field, value string, limit int) error {
	if err := required(field, value, limit); err != nil {
		return err
	}
	if strings.ContainsAny(value, "\r\n") {
		return fmt.Errorf("%s must be one line", field)
	}
	return nil
}
