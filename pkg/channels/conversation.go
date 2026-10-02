package channels

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Conversation is a Slack thread a service holds with one person through
// POST /conversations, on behalf of an agent the gateway does not run (a
// person's guide on their own machine): a direct message from the app whose
// thread is bound to the conversation. The service posts into it with
// POST /conversations/{id}/messages; a reply of the person's in the thread
// calls Reply.Tool through muster as the person, with Reply.Arguments plus
// "message", the reply as an A2A Message whose contextId is the
// conversation's id. The tool decides whom the person may reach; the gateway
// only delivers, and only the person's own replies.
type Conversation struct {
	// Person is the email of the person the conversation is with.
	Person string `json:"person"`
	// From names the agent the person talks to, shown under the opening
	// message.
	From string `json:"from"`
	// Text is the opening message, in Slack mrkdwn.
	Text  string         `json:"text"`
	Reply ToolInvocation `json:"reply"`
}

// ConversationMessage is a message the service posts into its conversation.
type ConversationMessage struct {
	Text string `json:"text"`
}

// ErrConversationNotFound is returned by PostConversationMessage for a
// conversation the gateway holds no record of: unknown, or quiet for longer
// than a conversation is kept.
var ErrConversationNotFound = errors.New("conversation not found")

// ConversationPoster opens conversations and posts into them.
type ConversationPoster interface {
	OpenConversation(ctx context.Context, conversation Conversation) (PostReceipt, error)
	// PostConversationMessage posts into the conversation's thread;
	// ErrConversationNotFound when the gateway holds no record of it.
	PostConversationMessage(ctx context.Context, id string, message ConversationMessage) (PostReceipt, error)
}

// Limits of a conversation: a message is one Slack message's text, the
// sender one line of its context.
const (
	ConversationTextMax = 12000
	ConversationFromMax = 200
)

// Validate reports the first field that would make the conversation
// unusable.
func (c Conversation) Validate() error {
	if !strings.Contains(c.Person, "@") {
		return fmt.Errorf("person %q is not an email", c.Person)
	}
	if err := requiredLine("from", c.From, ConversationFromMax); err != nil {
		return err
	}
	if err := required("text", c.Text, ConversationTextMax); err != nil {
		return err
	}
	if c.Reply.Tool == "" {
		return errors.New("reply.tool is required")
	}
	if _, ok := c.Reply.Arguments["message"]; ok {
		return errors.New(`reply.arguments: "message" is the gateway's: the person's reply`)
	}
	return nil
}

// Validate reports what makes the message unusable.
func (m ConversationMessage) Validate() error {
	return required("text", m.Text, ConversationTextMax)
}
