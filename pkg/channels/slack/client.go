package slack

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

const (
	slackAPIBase = "https://slack.com/api"
	// methodChatPostMessage is the Web API method for new posts; it is special
	// in two spots (display identity, forced unfurl-off).
	methodChatPostMessage = "chat.postMessage"
)

// slackHTTPClient bounds every Slack Web API call. Without a timeout a
// blackholed connection blocks the calling goroutine indefinitely; some call
// sites hold the per-thread slot while calling (e.g. the users.info lookup
// during dispatch), so an unbounded hang would wedge the thread until process
// restart.
var slackHTTPClient = &http.Client{Timeout: 30 * time.Second}

// slackAPIClient is a minimal HTTP client for the Slack Web API.
type slackAPIClient struct {
	botToken string
	baseURL  string
	// username / iconURL, when set, post under a custom display identity
	// (chat:write.customize). Applied only to chat.postMessage and
	// chat.postEphemeral (chat.update keeps the original message's identity).
	username string
	iconURL  string
}

// applyIdentity adds the client's display identity (username/icon_url) via set.
// It is a no-op unless an identity is configured and the method is one that
// honours chat:write.customize (a new post, not an edit). Each field is applied
// only when non-empty: a name without an icon posts under the custom name and
// the Slack app's own icon (Slack keeps the app icon when icon_url is omitted),
// which is what we want while the AgentCard exposes a name but no icon.
func (c *slackAPIClient) applyIdentity(method string, set func(k, v string)) {
	if c.username == "" && c.iconURL == "" {
		return
	}
	if method != methodChatPostMessage && method != "chat.postEphemeral" {
		return
	}
	if c.username != "" {
		set(paramUsername, c.username)
	}
	if c.iconURL != "" {
		set(paramIconURL, c.iconURL)
	}
}

func (c *slackAPIClient) postMessage(ctx context.Context, channel, text, threadTS string) (string, error) {
	params := url.Values{
		paramChannel: {channel},
		paramText:    {text},
	}
	if threadTS != "" {
		params.Set(paramThreadTS, threadTS)
	}
	return c.post(ctx, methodChatPostMessage, params)
}

// lookupUserEmail returns the email from the user's Slack profile.
// Falls back to the raw Slack user ID on any error so dispatch is never blocked.
// users.info is Tier-4 rate-limited, so the call goes through the same
// 429-retrying transport as every other Web API call.
func (c *slackAPIClient) lookupUserEmail(ctx context.Context, userID string) (string, error) {
	params := url.Values{paramUser: {userID}}
	body, err := c.call(ctx, "users.info", "application/x-www-form-urlencoded", params.Encode())
	if err != nil {
		return "", err
	}

	var result struct {
		OK   bool   `json:"ok"`
		Err  string `json:"error,omitempty"`
		User struct {
			Profile struct {
				Email string `json:"email"`
			} `json:"profile"`
		} `json:"user"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("slack users.info: decode: %w", err)
	}
	if !result.OK {
		return "", fmt.Errorf("slack users.info: %s", result.Err)
	}
	return result.User.Profile.Email, nil
}

// authTest returns the bot's own Slack user ID via auth.test, used to
// recognise the bot's own channel-join event.
func (c *slackAPIClient) authTest(ctx context.Context) (string, error) {
	body, err := c.call(ctx, "auth.test", "application/x-www-form-urlencoded", "")
	if err != nil {
		return "", err
	}

	var result struct {
		OK     bool   `json:"ok"`
		Err    string `json:"error,omitempty"`
		UserID string `json:"user_id"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("slack auth.test: decode: %w", err)
	}
	if !result.OK {
		return "", fmt.Errorf("slack auth.test: %s", result.Err)
	}
	return result.UserID, nil
}

// threadInitiatorScanLimit bounds the conversations.replies page scanned for the
// first human author. A thread that opens with more leading bot messages than
// this falls back to first-poster seeding.
const threadInitiatorScanLimit = 50

// threadInitiator returns the user ID of the earliest human (non-bot) author in
// a thread, via conversations.replies (messages are returned oldest-first). A
// bot-authored root is skipped: bot messages carry bot_id (and often a user
// field naming the bot's own user), so they are not a human initiator; the
// first message without bot_id is the human who effectively started the thread.
// Returns "" when the thread is empty or its scanned prefix is all bot messages.
func (c *slackAPIClient) threadInitiator(ctx context.Context, channel, threadTS string) (string, error) {
	params := url.Values{
		paramChannel: {channel},
		paramTS:      {threadTS},
		"limit":      {strconv.Itoa(threadInitiatorScanLimit)},
	}
	body, err := c.call(ctx, "conversations.replies", "application/x-www-form-urlencoded", params.Encode())
	if err != nil {
		return "", err
	}

	var result struct {
		OK       bool   `json:"ok"`
		Err      string `json:"error,omitempty"`
		Messages []struct {
			User  string `json:"user"`
			BotID string `json:"bot_id"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("slack conversations.replies: decode: %w", err)
	}
	if !result.OK {
		return "", fmt.Errorf("slack conversations.replies: %s", result.Err)
	}
	for _, m := range result.Messages {
		if m.BotID == "" && m.User != "" {
			return m.User, nil
		}
	}
	return "", nil
}

// errReactionsUnsupported reports that the bot cannot manage reactions (the
// reactions:write scope is missing, or the token type disallows it), so the
// caller should fall back to text-based progress.
var errReactionsUnsupported = errors.New("slack: reactions unsupported")

func (c *slackAPIClient) reactionsAdd(ctx context.Context, channel, ts, name string) error {
	return c.reaction(ctx, "reactions.add", channel, ts, name)
}

func (c *slackAPIClient) reactionsRemove(ctx context.Context, channel, ts, name string) error {
	return c.reaction(ctx, "reactions.remove", channel, ts, name)
}

func (c *slackAPIClient) reaction(ctx context.Context, method, channel, ts, name string) error {
	_, err := c.post(ctx, method, url.Values{
		paramChannel:   {channel},
		paramTimestamp: {ts},
		paramName:      {name},
	})
	if err != nil && (strings.Contains(err.Error(), "missing_scope") ||
		strings.Contains(err.Error(), "not_allowed_token_type")) {
		return errReactionsUnsupported
	}
	return err
}

// markdownBlocks wraps text in a single Block Kit markdown block, which renders
// Slack's supported Markdown (bold, italic, lists, tables, code blocks, ...)
// natively, without the mrkdwn conversion.
func markdownBlocks(md string) []any {
	return []any{map[string]any{bkType: bkMarkdown, bkText: md}}
}

// postMarkdown posts a new in-thread message rendered as a markdown block. The
// top-level text is the notification/accessibility fallback; it is mrkdwn-parsed
// by Slack, so agent output must be escaped there even though the markdown block
// itself must not be.
func (c *slackAPIClient) postMarkdown(ctx context.Context, channel, md, threadTS string) (string, error) {
	body := map[string]any{
		paramChannel: channel,
		paramText:    escapeMrkdwn(md),
		paramBlocks:  markdownBlocks(md),
	}
	if threadTS != "" {
		body[paramThreadTS] = threadTS
	}
	return c.postJSON(ctx, methodChatPostMessage, body)
}

// chatUpdateMarkdown replaces a message's content with a markdown block. The
// top-level fallback text is escaped for the same reason as in postMarkdown.
func (c *slackAPIClient) chatUpdateMarkdown(ctx context.Context, channel, ts, md string) error {
	body := map[string]any{
		paramChannel: channel,
		paramTS:      ts,
		paramText:    escapeMrkdwn(md),
		paramBlocks:  markdownBlocks(md),
	}
	_, err := c.postJSON(ctx, "chat.update", body)
	return err
}

// postApprovalPrompt posts a Block Kit message with ✅/❌ buttons for HITL
// approval. The button values encode the thread (routing) and the task the
// prompt renders (staleness check).
func (c *slackAPIClient) postApprovalPrompt(ctx context.Context, channel, threadID, taskID, promptText string) error {
	text := "_Waiting for approval…_"
	if promptText != "" {
		// promptText is agent-rendered (tool name, args, hint) and enters an
		// mrkdwn section block.
		text = truncateRunes(escapeMrkdwn(promptText), slackSectionTextMax)
	}
	body := map[string]any{
		paramChannel:  channel,
		paramThreadTS: threadID,
		paramText:     text,
		paramBlocks: []any{
			map[string]any{
				bkType: bkSection,
				bkText: map[string]any{bkType: bkMrkdwn, bkText: text},
			},
			map[string]any{
				bkType: bkActions,
				bkElements: []any{
					map[string]any{
						bkType:     bkButton,
						bkText:     map[string]any{bkType: bkPlainText, bkText: "✅ Approve"},
						bkStyle:    bkPrimary,
						bkActionID: hitlApprove,
						bkValue:    encodeHitlValue(threadID, taskID),
					},
					map[string]any{
						bkType:     bkButton,
						bkText:     map[string]any{bkType: bkPlainText, bkText: "❌ Deny"},
						bkStyle:    bkDanger,
						bkActionID: hitlDeny,
						bkValue:    encodeHitlValue(threadID, taskID),
					},
					map[string]any{
						bkType:     bkButton,
						bkText:     map[string]any{bkType: bkPlainText, bkText: "💬 Chat"},
						bkActionID: hitlChat,
						bkValue:    encodeHitlValue(threadID, taskID),
					},
				},
			},
		},
	}
	_, err := c.postJSON(ctx, methodChatPostMessage, body)
	return err
}

// questionSection renders an ask_user question as a bold mrkdwn section block.
// The question is agent-authored; the two asterisks count against Slack's
// 3000-char section limit.
func questionSection(question string) map[string]any {
	question = truncateRunes(escapeMrkdwn(question), slackSectionTextMax-2)
	return map[string]any{
		bkType: bkSection,
		bkText: map[string]any{bkType: bkMrkdwn, bkText: "*" + question + "*"},
	}
}

// submitActions renders the Submit button that commits a widget selection. Its
// value encodes the thread (routing) and the task the prompt renders
// (staleness check).
func submitActions(threadID, taskID string) map[string]any {
	return map[string]any{
		bkType: bkActions,
		bkElements: []any{
			map[string]any{
				bkType:     bkButton,
				bkText:     map[string]any{bkType: bkPlainText, bkText: "Submit"},
				bkStyle:    bkPrimary,
				bkActionID: hitlSubmit,
				bkValue:    encodeHitlValue(threadID, taskID),
			},
		},
	}
}

// choiceOptions builds the Block Kit option objects for a question's choices,
// each valued by its choice index. Labels are capped at the option-text limit;
// the caller routes longer labels to the section layout, so truncation never
// bites in practice.
func choiceOptions(choices []string) []any {
	options := make([]any, 0, len(choices))
	for i, choice := range choices {
		options = append(options, map[string]any{
			bkText:  map[string]any{bkType: bkPlainText, bkText: truncateRunes(choice, choiceLabelWidgetMax)},
			bkValue: strconv.Itoa(i),
		})
	}
	return options
}

// choiceWidgetBlock builds a radio_buttons (single-select) or checkboxes
// (multi-select) actions block for one question's choices. blockID lets the
// interaction handler locate the selection under state.values; every widget
// shares the hitlGroup action_id.
func choiceWidgetBlock(blockID string, choices []string, multiple bool) map[string]any {
	elementType := bkRadioButtons
	if multiple {
		elementType = bkCheckboxes
	}
	return map[string]any{
		bkType:    bkActions,
		bkBlockID: blockID,
		bkElements: []any{
			map[string]any{
				bkType:     elementType,
				bkActionID: hitlGroup,
				bkOptions:  choiceOptions(choices),
			},
		},
	}
}

// postChoiceWidgetPrompt posts an ask_user question as a vertical
// radio_buttons (single-select) or checkboxes (multi-select) widget plus a
// Submit button. Each option's value is its choice index; the interaction
// handler reads the selection out of state.values on Submit.
func (c *slackAPIClient) postChoiceWidgetPrompt(ctx context.Context, channel, threadID, taskID, question string, choices []string, multiple bool) error {
	body := map[string]any{
		paramChannel:  channel,
		paramThreadTS: threadID,
		paramText:     truncateRunes(escapeMrkdwn(question), slackSectionTextMax),
		paramBlocks: []any{
			questionSection(question),
			choiceWidgetBlock(hitlGroupBlock, choices, multiple),
			submitActions(threadID, taskID),
		},
	}
	_, err := c.postJSON(ctx, methodChatPostMessage, body)
	return err
}

// postChoiceFormPrompt posts a multi-question ask_user prompt as a single form:
// a question section plus a radio/checkbox widget per question, all committed by
// one Submit. Each question's widget block_id encodes its question index
// (hitlQGroupPrefix + "_<qi>") so the handler maps each selection back to its
// question. The caller (formRenderable) guarantees every question is widgetable.
func (c *slackAPIClient) postChoiceFormPrompt(ctx context.Context, channel, threadID, taskID string, questions []channels.HitlQuestion) error {
	blocks := make([]any, 0, 2*len(questions)+1)
	for qi, q := range questions {
		blocks = append(blocks, questionSection(q.Question))
		blocks = append(blocks, choiceWidgetBlock(fmt.Sprintf("%s_%d", hitlQGroupPrefix, qi), q.Choices, q.Multiple))
	}
	blocks = append(blocks, submitActions(threadID, taskID))
	body := map[string]any{
		paramChannel:  channel,
		paramThreadTS: threadID,
		paramText:     "Please answer the questions below.",
		paramBlocks:   blocks,
	}
	_, err := c.postJSON(ctx, "chat.postMessage", body)
	return err
}

// postChoiceSectionPrompt posts an ask_user question whose choices are too long
// for a widget option's 75-rune text: one section block per choice carries the
// full label (up to Slack's 3000-char section limit) with a selection control.
// Single-select uses an accessory button per row (a click commits, since one
// choice per row is unambiguous); multi-select uses an accessory single-option
// checkbox per row plus a Submit button, and the handler gathers the selected
// rows out of state.values.
func (c *slackAPIClient) postChoiceSectionPrompt(ctx context.Context, channel, threadID, taskID, question string, choices []string, multiple bool) error {
	blocks := []any{questionSection(question)}
	for i, choice := range choices {
		section := map[string]any{
			bkType: bkSection,
			bkText: map[string]any{bkType: bkMrkdwn, bkText: truncateRunes(escapeMrkdwn(choice), slackSectionTextMax)},
		}
		if multiple {
			section[bkBlockID] = fmt.Sprintf("%s_%d", hitlGroupBlock, i)
			section[bkAccessory] = map[string]any{
				bkType:     bkCheckboxes,
				bkActionID: hitlGroup,
				bkOptions: []any{
					map[string]any{
						bkText:  map[string]any{bkType: bkPlainText, bkText: "Select"},
						bkValue: strconv.Itoa(i),
					},
				},
			}
		} else {
			section[bkAccessory] = map[string]any{
				bkType:     bkButton,
				bkText:     map[string]any{bkType: bkPlainText, bkText: "Select"},
				bkActionID: fmt.Sprintf("%s_%d", hitlChoice, i),
				bkValue:    encodeChoiceValue(threadID, taskID, i),
			}
		}
		blocks = append(blocks, section)
	}
	if multiple {
		blocks = append(blocks, submitActions(threadID, taskID))
	}
	body := map[string]any{
		paramChannel:  channel,
		paramThreadTS: threadID,
		paramText:     truncateRunes(escapeMrkdwn(question), slackSectionTextMax),
		paramBlocks:   blocks,
	}
	_, err := c.postJSON(ctx, "chat.postMessage", body)
	return err
}

// postSignInPrompt posts a Block Kit message with a "Sign in" button linking
// to linkURL, and returns the posted message's ts. It is used to
// nudge an unlinked Slack user into the OBO account-linking flow. A real
// threaded message, never an ephemeral: for a root channel mention the prompt
// is the thread's first visible reply (a thread-scoped ephemeral there is never
// surfaced by Slack), in an assistant DM only thread replies render in the
// assistant pane, and the returned ts lets the prompt be rewritten in place
// once the link completes.
func (c *slackAPIClient) postSignInPrompt(ctx context.Context, channel, threadID, user, linkURL string) (string, error) {
	// The prompt is a public thread message and its link is minted for one
	// user: address it, or a bystander clicks a button bound to someone else's
	// identity and lands on the email-mismatch page.
	text := "Sign in so I can act as you. " +
		"Until you do, I can't run tools on your behalf."
	if user != "" {
		text = "<@" + user + "> " + text
	}
	body := map[string]any{
		paramChannel: channel,
		paramText:    text,
		paramBlocks: []any{
			map[string]any{
				bkType: bkSection,
				bkText: map[string]any{bkType: bkMrkdwn, bkText: text},
			},
			map[string]any{
				bkType: bkActions,
				bkElements: []any{
					map[string]any{
						bkType:     bkButton,
						bkText:     map[string]any{bkType: bkPlainText, bkText: "Sign in"},
						bkStyle:    bkPrimary,
						bkActionID: oboSignIn,
						bkURL:      linkURL,
					},
				},
			},
		},
	}
	if threadID != "" {
		body[paramThreadTS] = threadID
	}
	return c.postJSON(ctx, methodChatPostMessage, body)
}

// slackSectionTextMax is Slack's limit on a section block's text object; a
// longer text gets the whole message rejected with invalid_blocks.
const slackSectionTextMax = 3000

// postConnectorPrompt posts an ephemeral (target-user-only) Block Kit message
// offering to connect a muster backend the agent cannot use for the user yet:
// a "Connect <server>" URL button opening loginURL plus a "Not now" dismissal.
// When threadID is set the prompt is posted in-thread.
func (c *slackAPIClient) postConnectorPrompt(ctx context.Context, channel, threadID, user, server, loginURL string) error {
	text := fmt.Sprintf("The agent can't use *%s* for you yet. Connect your account once so those tools work.", escapeMrkdwn(server))
	body := map[string]any{
		paramChannel: channel,
		paramUser:    user,
		paramText:    text,
		paramBlocks: []any{
			map[string]any{
				bkType: bkSection,
				bkText: map[string]any{bkType: bkMrkdwn, bkText: text},
			},
			map[string]any{
				bkType: bkActions,
				bkElements: []any{
					map[string]any{
						bkType:     bkButton,
						bkText:     map[string]any{bkType: bkPlainText, bkText: truncateButtonLabel("Connect " + server)},
						bkStyle:    bkPrimary,
						bkActionID: connectorConnect,
						bkValue:    server,
						bkURL:      loginURL,
					},
					map[string]any{
						bkType:     bkButton,
						bkText:     map[string]any{bkType: bkPlainText, bkText: "Not now"},
						bkActionID: connectorDismiss,
						bkValue:    server,
					},
				},
			},
		},
	}
	if threadID != "" {
		body[paramThreadTS] = threadID
	}
	_, err := c.postJSON(ctx, "chat.postEphemeral", body)
	return err
}

// postEphemeralText posts a plain in-thread message visible only to user.
func (c *slackAPIClient) postEphemeralText(ctx context.Context, channel, user, threadTS, text string) error {
	body := map[string]any{
		paramChannel: channel,
		paramUser:    user,
		paramText:    text,
	}
	if threadTS != "" {
		body[paramThreadTS] = threadTS
	}
	_, err := c.postJSON(ctx, "chat.postEphemeral", body)
	return err
}

// postAccessConsentPrompt posts the ephemeral (initiator-only) "is <newcomer>
// allowed?" prompt with Yes/No buttons. Only the initiator receives it, so only
// the initiator can click. The button value encodes the thread and the newcomer
// so the interaction handler resolves the right parked request.
func (c *slackAPIClient) postAccessConsentPrompt(ctx context.Context, channel, threadID, initiator, newcomer string) error {
	// The grant lets the newcomer instruct the agent under their own identity
	// and session; it does not delegate the initiator's, so the wording must
	// not promise "on your behalf".
	text := fmt.Sprintf("Is <@%s> allowed to instruct the agent in this thread?", newcomer)
	value := encodeAccessValue(threadID, newcomer)
	body := map[string]any{
		paramChannel:  channel,
		paramUser:     initiator,
		paramThreadTS: threadID,
		paramText:     text,
		paramBlocks: []any{
			map[string]any{
				bkType: bkSection,
				bkText: map[string]any{bkType: bkMrkdwn, bkText: text},
			},
			map[string]any{
				bkType: bkActions,
				bkElements: []any{
					map[string]any{
						bkType:     bkButton,
						bkText:     map[string]any{bkType: bkPlainText, bkText: "✅ Yes"},
						bkStyle:    bkPrimary,
						bkActionID: accessAllow,
						bkValue:    value,
					},
					map[string]any{
						bkType:     bkButton,
						bkText:     map[string]any{bkType: bkPlainText, bkText: "❌ No"},
						bkStyle:    bkDanger,
						bkActionID: accessDeny,
						bkValue:    value,
					},
				},
			},
		},
	}
	_, err := c.postJSON(ctx, "chat.postEphemeral", body)
	return err
}

// interactionHTTPClient bounds POSTs to a Slack interaction response_url. These
// run on the adapter's long-lived context (routeInteraction), so without a
// timeout a hung upstream would park the goroutine until process shutdown.
var interactionHTTPClient = &http.Client{Timeout: 10 * time.Second}

// respondURL replaces a message via a Slack interaction response_url. Ephemeral
// messages have no addressable ts for chat.update, so the access-consent prompt
// is updated this way after a click. The response_url is unauthenticated and
// short-lived; a failure is non-fatal (the decision has already been recorded).
func respondURL(ctx context.Context, responseURL, threadTS, text string) error {
	if responseURL == "" {
		return nil
	}
	payload := map[string]any{
		"replace_original": true,
		"response_type":    "ephemeral",
		paramText:          text,
	}
	// A response_url replacement of a thread-scoped ephemeral must carry the
	// thread_ts of the source, or Slack renders the replacement at channel
	// top level as well as in the thread.
	if threadTS != "" {
		payload[paramThreadTS] = threadTS
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("slack respond_url: marshal: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, responseURL, strings.NewReader(string(body)))
	if err != nil {
		return fmt.Errorf("slack respond_url: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	resp, err := interactionHTTPClient.Do(req) //nolint:gosec
	if err != nil {
		return fmt.Errorf("slack respond_url: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf("slack respond_url: http status %d", resp.StatusCode)
	}
	return nil
}

// truncateButtonLabel keeps a button label within Slack's 75-character limit.
func truncateButtonLabel(s string) string {
	return truncateRunes(s, 75)
}

// truncateRunes caps s at max runes, replacing the tail with an ellipsis.
// Counting runes (not bytes) means a multi-byte glyph is never split mid-rune.
func truncateRunes(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}

// chatUpdateBlocks replaces a Block Kit message with plain text (used to mark
// an approval decision after the user clicks a button).
func (c *slackAPIClient) chatUpdateBlocks(ctx context.Context, channel, ts, text string) error {
	body := map[string]any{
		paramChannel: channel,
		paramTS:      ts,
		paramText:    text,
		paramBlocks:  []any{},
	}
	_, err := c.postJSON(ctx, "chat.update", body)
	return err
}

func (c *slackAPIClient) postJSON(ctx context.Context, method string, body any) (string, error) {
	// The identity fields go onto a clone so the caller's map stays untouched.
	if m, ok := body.(map[string]any); ok {
		cloned := maps.Clone(m)
		c.applyIdentity(method, func(k, v string) {
			if _, exists := cloned[k]; !exists {
				cloned[k] = v
			}
		})
		if method == methodChatPostMessage {
			// Bot posts relay agent- and tool-controlled links; an unfurl has
			// Slack's crawler fetch them, which for single-use auth links can
			// trip the auth server's replay detection.
			cloned[paramUnfurlLinks] = false
			cloned[paramUnfurlMedia] = false
		}
		body = cloned
	}
	data, err := json.Marshal(body)
	if err != nil {
		return "", fmt.Errorf("slack %s: marshal: %w", method, err)
	}
	return c.send(ctx, method, "application/json; charset=utf-8", string(data))
}

type slackResponse struct {
	OK    bool   `json:"ok"`
	Ts    string `json:"ts"`
	Error string `json:"error,omitempty"`
}

func (c *slackAPIClient) post(ctx context.Context, method string, params url.Values) (string, error) {
	c.applyIdentity(method, params.Set)
	if method == methodChatPostMessage {
		params.Set(paramUnfurlLinks, "false")
		params.Set(paramUnfurlMedia, "false")
	}
	return c.send(ctx, method, "application/x-www-form-urlencoded", params.Encode())
}

// rateLimitRetryCap bounds how long a Retry-After pause may hold a call;
// a longer server-requested wait fails the call instead of stalling the writer.
const rateLimitRetryCap = 30 * time.Second

// send executes one Slack Web API call and returns the ts of the affected
// message, for methods whose response carries one.
func (c *slackAPIClient) send(ctx context.Context, method, contentType, payload string) (string, error) {
	body, err := c.call(ctx, method, contentType, payload)
	if err != nil {
		return "", err
	}
	var result slackResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("slack %s: decode response: %w", method, err)
	}
	if !result.OK {
		return "", fmt.Errorf("slack %s: %s", method, result.Error)
	}
	return result.Ts, nil
}

// call executes one Slack Web API POST and returns the raw response body. A
// 429 is retried honoring Retry-After: rate limiting is a pacing signal, not a
// turn-fatal error, and a multi-chunk flush plus tool posts can draw several
// consecutive 429s against chat.postMessage's ~1 msg/sec/channel limit. A
// Retry-After longer than rateLimitRetryCap, or the attempt budget running
// out, fails the call rather than waiting it out. Any other non-2xx status is
// an error carrying the status code, not a JSON decode attempt on a non-API
// body.
func (c *slackAPIClient) call(ctx context.Context, method, contentType, payload string) ([]byte, error) {
	const maxAttempts = 4
	for attempt := 1; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/"+method, strings.NewReader(payload))
		if err != nil {
			return nil, fmt.Errorf("slack %s: build request: %w", method, err)
		}
		req.Header.Set("Content-Type", contentType)
		req.Header.Set("Authorization", "Bearer "+c.botToken)

		resp, err := slackHTTPClient.Do(req) //nolint:gosec
		if err != nil {
			return nil, fmt.Errorf("slack %s: %w", method, err)
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			_ = resp.Body.Close()
			wait := retryAfter(resp.Header)
			if attempt >= maxAttempts || wait > rateLimitRetryCap {
				return nil, fmt.Errorf("slack %s: rate limited (retry after %s)", method, wait)
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(wait):
			}
			continue
		}
		if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
			_ = resp.Body.Close()
			return nil, fmt.Errorf("slack %s: http status %d", method, resp.StatusCode)
		}

		body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("slack %s: read response: %w", method, err)
		}
		return body, nil
	}
}

// retryAfter reads the Retry-After header of a 429 response, defaulting to 1s
// when absent or unparsable.
func retryAfter(header http.Header) time.Duration {
	seconds, err := strconv.Atoi(header.Get("Retry-After"))
	if err != nil || seconds < 0 {
		return time.Second
	}
	return time.Duration(seconds) * time.Second
}
