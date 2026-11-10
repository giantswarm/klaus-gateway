package slack_test

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
	slackadapter "github.com/giantswarm/klaus-gateway/pkg/channels/slack"
	"github.com/giantswarm/klaus-gateway/pkg/muster"
	"github.com/giantswarm/klaus-gateway/pkg/muster/mustertest"
)

const sessionConnectLink = "https://muster.example/oauth/proxy/start?state=abc"

func sessionConnectChallenge() *pkga2a.ConnectRequiredError {
	return &pkga2a.ConnectRequiredError{SignIn: &muster.SignIn{Message: "Sign in to agent-manager to continue.", URL: sessionConnectLink}}
}

// sessionConnectAdapter is connectorAdapter in channel mode with the adapter's
// log captured, accepter accepting sign-ins, and a users.info that answers the
// person's e-mail, so the turn rewrites the sender the way it does on an
// installation.
func sessionConnectAdapter(t *testing.T, gw *stubGateway, accepter *fakeAccepter, opts ...func(*slackadapter.Adapter)) (*fakeSlackAPI, *httptest.Server, *syncBuffer) {
	t.Helper()
	fake := newFakeSlackAPI()
	fake.setResponse("users.info", `{"ok":true,"user":{"id":"U1","profile":{"email":"u1@example.com"}}}`)
	logs := &syncBuffer{}
	opts = append([]func(*slackadapter.Adapter){channelMode, func(a *slackadapter.Adapter) {
		a.OBO = &fakeOBO{linkedUser: "U1", token: "tok"}
		a.ConnectorPrompts = true
		a.SignIns = accepter
		a.Logger = slog.New(slog.NewTextHandler(logs, nil))
	}}, opts...)
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, opts...)
	return fake, srv, logs
}

// logLine is the first captured log line that carries every needle.
func logLine(logs *syncBuffer, needles ...string) string {
	for _, line := range strings.Split(logs.String(), "\n") {
		found := true
		for _, needle := range needles {
			if !strings.Contains(line, needle) {
				found = false
				break
			}
		}
		if found {
			return line
		}
	}
	return ""
}

// connectButtonIn walks calls for the Connect button and returns its url and
// value, with the call it was found in.
func connectButtonIn(calls []recordedCall) (buttonURL, buttonValue string, call recordedCall, ok bool) {
	for _, c := range calls {
		blocks, _ := c.params["blocks"].([]any)
		for _, block := range blocks {
			blockMap, _ := block.(map[string]any)
			elements, _ := blockMap["elements"].([]any)
			for _, element := range elements {
				button, _ := element.(map[string]any)
				if button["action_id"] == "connector_connect" {
					buttonURL, _ = button["url"].(string)
					buttonValue, _ = button["value"].(string)
					return buttonURL, buttonValue, c, true
				}
			}
		}
	}
	return "", "", recordedCall{}, false
}

// postsTo are the chat.postMessage calls into channel.
func postsTo(fake *fakeSlackAPI, channel string) []recordedCall {
	var calls []recordedCall
	for _, c := range fake.pathCalls("chat.postMessage") {
		if c.params["channel"] == channel {
			calls = append(calls, c)
		}
	}
	return calls
}

// updatesIn are the chat.update calls into channel.
func updatesIn(fake *fakeSlackAPI, channel string) []recordedCall {
	var calls []recordedCall
	for _, c := range fake.pathCalls("chat.update") {
		if c.params["channel"] == channel {
			calls = append(calls, c)
		}
	}
	return calls
}

// A Session start muster asks a sign-in for, in a channel thread: the Connect
// prompt goes to the person as an ephemeral, the same card goes to their DM
// led by where it was asked, and a line everyone sees says whom the thread
// waits for and where the button is. No failure note. Every button opens
// muster's link unchanged and carries the completion state. The person's click
// accepts the sign-in as them, rewrites the ephemeral, the line and the card,
// and runs the held message as the thread's first turn, with nothing retyped,
// also when the turn had resolved the sender to their e-mail.
func TestSessionConnect_ChannelThreadPromptsEverywhereAndRunsTheHeldMessage(t *testing.T) {
	var dispatched struct {
		mu   sync.Mutex
		msgs []channels.InboundMessage
	}
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok"}, {Done: true}}}
	gw.dispatchErr = sessionConnectChallenge()
	gw.onDispatch = func(msg channels.InboundMessage) {
		dispatched.mu.Lock()
		dispatched.msgs = append(dispatched.msgs, msg)
		dispatched.mu.Unlock()
	}
	accepter := &fakeAccepter{}
	fake, srv, logs := sessionConnectAdapter(t, gw, accepter)
	responseSrv, capturedBody := newResponseURLCapture(t)

	gw.openConversation(t, "C1", "140.000", "U1", "test-agent")
	sendEvent(t, srv, mention("U1", "how many nodes in gazelle?", "140.000", ""))

	var buttonURL, stateID string
	var ok bool
	require.Eventually(t, func() bool {
		buttonURL, stateID, ok = connectButton(fake)
		return ok
	}, flowWait, 20*time.Millisecond, "the ephemeral Connect prompt is posted")
	ephemeral := allText(fake.pathCalls("chat.postEphemeral"))
	require.Contains(t, ephemeral, "*muster* asks you to sign in once:\n>Sign in to agent-manager to continue.")
	require.Contains(t, ephemeral, "Opens *muster.example*\n`"+sessionConnectLink+"`", "the whole link, its domain in bold")
	require.Contains(t, ephemeral, "your message goes to the agent once you're signed in")
	require.Equal(t, sessionConnectLink, buttonURL, "muster's link, unchanged")
	require.NotEmpty(t, stateID, "the button value is the completion state")

	dmURL, dmValue, dmCard, ok := connectButtonIn(postsTo(fake, "U1"))
	require.True(t, ok, "the same card goes to the person's DM")
	require.Equal(t, sessionConnectLink, dmURL)
	require.Equal(t, stateID, dmValue, "the card's button carries the same completion state")
	require.Contains(t, fmt.Sprint(dmCard.params["blocks"]), "Asked in a thread in <#C1>.", "the card says where it was asked")
	require.Equal(t, false, dmCard.params["unfurl_links"], "Slack does not fetch the sign-in link")
	require.Equal(t, false, dmCard.params["unfurl_media"])
	require.Len(t, postsTo(fake, "U1"), 1)

	line := allText(postsTo(fake, "C1"))
	require.Contains(t, line, "Waiting for <@U1> to sign in (the Sign in button is in a direct message from the app)")
	require.NotContains(t, line, "muster.example", "the line carries no link")
	require.NotContains(t, line, "The turn failed", "a consent to give is not a failure")
	for _, c := range postsTo(fake, "C1") {
		require.Equal(t, "140.000", c.params["thread_ts"], "the line is in the thread")
	}
	require.Equal(t, 1, gw.dispatchCount())
	dispatched.mu.Lock()
	require.Equal(t, "u1@example.com", dispatched.msgs[0].Subject, "the turn ran under the resolved e-mail")
	dispatched.mu.Unlock()

	record := logLine(logs, "record=session_connect_required")
	require.Contains(t, record, "slack_user=U1")
	complete := logLine(logs, "record=turn_complete")
	require.Contains(t, complete, "outcome=connect_required")
	require.NotContains(t, complete, "failure_class", "a consent to give is no failure class")
	require.Zero(t, accepter.count(), "nothing is accepted before the click")

	// Signed in: the next start succeeds.
	gw.mu.Lock()
	gw.dispatchErr = nil
	gw.mu.Unlock()
	sendConnectorInteractionURL(t, srv, "connector_connect", stateID, responseSrv.URL)

	require.Eventually(t, func() bool { return accepter.count() == 1 }, flowWait, 20*time.Millisecond, "the click accepts the sign-in")
	accepter.mu.Lock()
	require.Equal(t, sessionConnectLink, accepter.accepted[0].URL, "the sign-in the start was asked")
	require.Equal(t, "tok", accepter.bearers[0], "as the person")
	accepter.mu.Unlock()
	require.Eventually(t, func() bool {
		return strings.Contains(capturedBody(), "Opened the sign-in link.")
	}, flowWait, 20*time.Millisecond, "the clicked ephemeral says what the click started")
	require.Eventually(t, func() bool {
		return strings.Contains(allText(updatesIn(fake, "C1")), "<@U1> signed in.") &&
			strings.Contains(allText(updatesIn(fake, "U1")), "Signed in. Your message is on its way to the agent.")
	}, flowWait, 20*time.Millisecond, "the line and the DM card are rewritten")
	require.Eventually(t, func() bool {
		dispatched.mu.Lock()
		defer dispatched.mu.Unlock()
		return len(dispatched.msgs) == 2
	}, flowWait, 20*time.Millisecond, "the held message runs once the person has signed in")
	dispatched.mu.Lock()
	replayed := dispatched.msgs[1]
	dispatched.mu.Unlock()
	require.Equal(t, "how many nodes in gazelle?", replayed.Text, "the held message itself, not a synthetic continuation")
	require.Equal(t, "u1@example.com", replayed.Subject, "admitted again as the person, not as their e-mail")
	require.Equal(t, "U1", replayed.SenderID)
	require.NotContains(t, allText(fake.pathCalls("chat.postEphemeral")), "Sign in to Giant Swarm", "no sign-in prompt: the replay knows the person")
	require.Eventually(t, func() bool {
		return strings.Contains(fake.streamedText(), "ok")
	}, flowWait, 20*time.Millisecond, "the agent's answer lands in the thread")
	require.Len(t, updatesIn(fake, "C1"), 1, "the replayed turn finds nothing left to settle")

	sendConnectorInteractionURL(t, srv, "connector_connect", stateID, responseSrv.URL)
	time.Sleep(100 * time.Millisecond)
	require.Equal(t, 1, accepter.count(), "a second click accepts nothing more")
	require.Equal(t, 2, gw.dispatchCount(), "and runs the held message no second time")
}

// A second message in the thread before the person connects posts the
// ephemeral again, and refreshes the DM card in place instead of stacking
// cards; the line, unchanged, is left alone.
func TestSessionConnect_RepromptRefreshesTheCard(t *testing.T) {
	gw := &stubGateway{dispatchErr: sessionConnectChallenge()}
	fake, srv, _ := sessionConnectAdapter(t, gw, &fakeAccepter{})

	gw.openConversation(t, "C1", "150.000", "U1", "test-agent")
	sendEvent(t, srv, mention("U1", "how many nodes in gazelle?", "150.000", ""))
	require.Eventually(t, func() bool { _, _, ok := connectButton(fake); return ok },
		flowWait, 20*time.Millisecond, "the first prompt is posted")
	_, firstState, _, ok := connectButtonIn(postsTo(fake, "U1"))
	require.True(t, ok)

	sendEvent(t, srv, mention("U1", "still there?", "151.000", "150.000"))
	require.Eventually(t, func() bool { return len(fake.pathCalls("chat.postEphemeral")) == 2 },
		flowWait, 20*time.Millisecond, "the second message gets the ephemeral again")
	require.Len(t, postsTo(fake, "U1"), 1, "the DM holds one card")
	require.Len(t, postsTo(fake, "C1"), 1, "the thread holds one line")

	var refreshedState string
	require.Eventually(t, func() bool {
		_, refreshedState, _, ok = connectButtonIn(updatesIn(fake, "U1"))
		return ok
	}, flowWait, 20*time.Millisecond, "the DM card is refreshed")
	require.NotEqual(t, firstState, refreshedState, "with the fresh completion state")
	require.Empty(t, updatesIn(fake, "C1"), "the line reads the same, so it is not rewritten")
}

// A person who connected some other way (another thread's prompt, another
// muster client) sends again: the start goes through, and the thread's line
// and the DM card settle without any click. The held message stays unsent,
// so the card does not say it is on its way, and the stale button's click
// runs nothing.
func TestSessionConnect_NextTurnSettlesTheSurfaces(t *testing.T) {
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok"}, {Done: true}}}
	gw.dispatchErr = sessionConnectChallenge()
	accepter := &fakeAccepter{}
	fake, srv, _ := sessionConnectAdapter(t, gw, accepter)

	gw.openConversation(t, "C1", "170.000", "U1", "test-agent")
	sendEvent(t, srv, mention("U1", "how many nodes in gazelle?", "170.000", ""))
	var stateID string
	require.Eventually(t, func() bool {
		var ok bool
		_, stateID, ok = connectButton(fake)
		return ok
	}, flowWait, 20*time.Millisecond, "the prompt is posted")

	gw.mu.Lock()
	gw.dispatchErr = nil
	gw.mu.Unlock()
	sendEvent(t, srv, mention("U1", "connected, try again", "171.000", "170.000"))
	require.Eventually(t, func() bool {
		return strings.Contains(allText(updatesIn(fake, "C1")), "<@U1> signed in.") &&
			strings.Contains(allText(updatesIn(fake, "U1")), "Signed in.")
	}, flowWait, 20*time.Millisecond, "the line and the card settle on the turn that went through")
	require.NotContains(t, allText(updatesIn(fake, "U1")), "on its way", "the held message stays unsent")
	require.Eventually(t, func() bool { return strings.Contains(fake.streamedText(), "ok") },
		flowWait, 20*time.Millisecond, "the agent answers")

	sendConnectorInteractionURL(t, srv, "connector_connect", stateID, "")
	time.Sleep(100 * time.Millisecond)
	require.Zero(t, accepter.count(), "the stale prompt's state is gone")
	require.Equal(t, 2, gw.dispatchCount(), "the held message is not run after the fact")
}

// musterSignIn is the sign-in a muster client reads when muster answers a
// call of tool with one.
func musterSignIn(t *testing.T, tool string) *muster.SignIn {
	t.Helper()
	fake := mustertest.New(map[string]func(map[string]any) mustertest.Answer{
		tool: func(map[string]any) mustertest.Answer {
			return mustertest.Answer{SignIn: &mustertest.SignIn{Message: "Sign in to continue.", URL: sessionConnectLink}}
		},
	})
	t.Cleanup(fake.Close)
	res, err := muster.NewClient(fake.URL).CallTool(t.Context(), "tok", tool, nil)
	require.NoError(t, err)
	require.NotNil(t, res.SignIn)
	return res.SignIn
}

// The sign-in is the person's, not the thread's: a person who started two
// threads before signing in has a prompt, a line, a DM card and a held message
// in each. The click on either prompt, once the accept ran its start, settles
// both lines and both cards and runs each held message as the person in its
// own thread, once; a stale button of either thread then runs nothing. The
// person's thread waiting on another tool and another person's thread are not
// touched.
func TestSessionConnect_SignInSettlesThePersonsOtherThreads(t *testing.T) {
	for _, clickedThread := range []string{"300.000", "310.000"} {
		t.Run("click in thread "+clickedThread, func(t *testing.T) {
			var dispatched struct {
				mu   sync.Mutex
				msgs []channels.InboundMessage
			}
			startSignIn := &pkga2a.ConnectRequiredError{SignIn: musterSignIn(t, "x_agent-manager_start_session")}
			otherSignIn := &pkga2a.ConnectRequiredError{SignIn: musterSignIn(t, "x_other-manager_start_session")}
			gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok"}, {Done: true}}}
			gw.dispatchErr = startSignIn
			gw.onDispatch = func(msg channels.InboundMessage) {
				dispatched.mu.Lock()
				dispatched.msgs = append(dispatched.msgs, msg)
				dispatched.mu.Unlock()
			}
			accepter := &fakeAccepter{}
			fake, srv, _ := sessionConnectAdapter(t, gw, accepter, func(a *slackadapter.Adapter) {
				a.OBO = perUserOBO{tokens: map[string]string{"U1": "tok", "U2": "tok-2"}}
			})
			challenge := func(signIn *pkga2a.ConnectRequiredError) {
				gw.mu.Lock()
				gw.dispatchErr = signIn
				gw.mu.Unlock()
			}
			mentionAndWaitForPrompt := func(user, text, threadTS string, prompts int) {
				gw.openConversation(t, "C1", threadTS, user, "test-agent")
				sendEvent(t, srv, mention(user, text, threadTS, ""))
				require.Eventually(t, func() bool { return len(fake.pathCalls("chat.postEphemeral")) == prompts },
					flowWait, 20*time.Millisecond, "the prompt of thread %s is posted", threadTS)
			}
			mentionAndWaitForPrompt("U1", "how many nodes in gazelle?", "300.000", 1)
			mentionAndWaitForPrompt("U1", "and in glean?", "310.000", 2)
			challenge(otherSignIn)
			mentionAndWaitForPrompt("U1", "list my repositories", "320.000", 3)
			challenge(startSignIn)
			mentionAndWaitForPrompt("U2", "who is on call?", "330.000", 4)
			require.Len(t, postsTo(fake, "U1"), 3, "a DM card for each of the person's threads")
			require.Len(t, postsTo(fake, "U2"), 1)

			states := map[string]string{}
			for _, c := range fake.pathCalls("chat.postEphemeral") {
				_, value, _, ok := connectButtonIn([]recordedCall{c})
				require.True(t, ok)
				threadTS, _ := c.params["thread_ts"].(string)
				states[threadTS] = value
			}

			gw.mu.Lock()
			gw.dispatchErr = nil
			gw.mu.Unlock()
			sendConnectorInteractionURL(t, srv, "connector_connect", states[clickedThread], "")

			signedInLines := func() []recordedCall {
				var calls []recordedCall
				for _, c := range updatesIn(fake, "C1") {
					if strings.Contains(allText([]recordedCall{c}), "<@U1> signed in.") {
						calls = append(calls, c)
					}
				}
				return calls
			}
			require.Eventually(t, func() bool {
				return len(signedInLines()) == 2 &&
					strings.Count(allText(updatesIn(fake, "U1")), "Signed in. Your message is on its way to the agent.") == 2
			}, flowWait, 20*time.Millisecond, "both lines and both cards are rewritten")
			require.NotEqual(t, signedInLines()[0].params["ts"], signedInLines()[1].params["ts"], "each thread's own line")
			require.Eventually(t, func() bool {
				dispatched.mu.Lock()
				defer dispatched.mu.Unlock()
				return len(dispatched.msgs) == 6
			}, flowWait, 20*time.Millisecond, "both held messages run")
			dispatched.mu.Lock()
			replays := map[string]channels.InboundMessage{}
			for _, msg := range dispatched.msgs[4:] {
				replays[msg.ThreadID] = msg
			}
			dispatched.mu.Unlock()
			require.Len(t, replays, 2, "each held message once, in its own thread")
			require.Equal(t, "how many nodes in gazelle?", replays["300.000"].Text)
			require.Equal(t, "and in glean?", replays["310.000"].Text)
			for _, msg := range replays {
				require.Equal(t, "U1", msg.SenderID, "run as the person")
			}
			require.Equal(t, 1, accepter.count(), "only the clicked prompt's sign-in is accepted")

			for _, thread := range []string{"300.000", "310.000"} {
				sendConnectorInteractionURL(t, srv, "connector_connect", states[thread], "")
			}
			time.Sleep(100 * time.Millisecond)
			require.Equal(t, 1, accepter.count(), "a stale button accepts nothing")
			require.Equal(t, 6, gw.dispatchCount(), "and runs nothing")
			require.Len(t, updatesIn(fake, "C1"), 2, "each line is rewritten once, the other tool's line not at all")
			require.Len(t, updatesIn(fake, "U1"), 2, "the other tool's card is not touched")
			require.Empty(t, updatesIn(fake, "U2"), "another person's card is not touched")
		})
	}
}

// In a direct message with the app the Connect card is a message of the
// conversation: one reader, so no ephemeral, no DM card and no line. A later
// message gets a card of its own and the earlier one loses its button. The
// card's click accepts the sign-in, rewrites the card in place and runs the
// held message; nothing goes through the card's response_url.
func TestSessionConnect_DMPostsTheCardAsAMessage(t *testing.T) {
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok"}, {Done: true}}}
	gw.dispatchErr = sessionConnectChallenge()
	accepter := &fakeAccepter{}
	fake := newFakeSlackAPI()
	_, srv := newEventsAdapter(t, gw, fake.server(t).URL, func(a *slackadapter.Adapter) {
		a.OBO = &fakeOBO{linkedUser: "U1", token: "tok"}
		a.ConnectorPrompts = true
		a.SignIns = accepter
	})
	responseSrv, capturedBody := newResponseURLCapture(t)

	gw.openConversation(t, "D1", "160.000", "U1", "test-agent")
	sendEvent(t, srv, dmEvent("U1", "how many nodes in gazelle?", "160.000"))

	var firstState string
	var card recordedCall
	var ok bool
	require.Eventually(t, func() bool {
		_, firstState, card, ok = connectButtonIn(fake.pathCalls("chat.postMessage"))
		return ok
	}, flowWait, 20*time.Millisecond, "the card is posted as a message")
	require.Equal(t, "D1", card.params["channel"])
	require.Contains(t, allText([]recordedCall{card}), "*muster* asks you to sign in once:")
	require.NotContains(t, fmt.Sprint(card.params["blocks"]), "Asked in", "the conversation is where it was asked")
	require.Empty(t, fake.pathCalls("chat.postEphemeral"), "no ephemeral in a DM")
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), "Waiting for", "no line in a DM")

	sendEvent(t, srv, dmThreadEvent("U1", "still there?", "161.000", "160.000"))
	require.Eventually(t, func() bool { return len(postsTo(fake, "D1")) == 2 },
		flowWait, 20*time.Millisecond, "the later message gets a card of its own under it")
	require.Eventually(t, func() bool {
		return strings.Contains(allText(updatesIn(fake, "D1")), "Sign in with the newer prompt below.")
	}, flowWait, 20*time.Millisecond, "the earlier card loses its button")
	_, _, _, hasButton := connectButtonIn(updatesIn(fake, "D1"))
	require.False(t, hasButton)
	secondURL, stateID, _, ok := connectButtonIn(postsTo(fake, "D1")[1:])
	require.True(t, ok)
	require.Equal(t, sessionConnectLink, secondURL)
	require.NotEqual(t, firstState, stateID)

	gw.mu.Lock()
	gw.dispatchErr = nil
	gw.mu.Unlock()
	sendClick(t, srv, "connector_connect", stateID, responseSrv.URL, false)
	require.Eventually(t, func() bool {
		return strings.Contains(allText(updatesIn(fake, "D1")), "Signed in. Your message is on its way to the agent.")
	}, flowWait, 20*time.Millisecond, "the card is rewritten in place")
	require.Eventually(t, func() bool { return gw.dispatchCount() == 3 },
		flowWait, 20*time.Millisecond, "the held message runs")
	require.Equal(t, 1, accepter.count())
	require.Empty(t, capturedBody(), "nothing is sent through the card's response_url")
}

// A collaborator's turn that starts the thread owner's Session meets the
// owner's challenge: the link is the owner's to follow, so the collaborator
// gets a private note naming them and no button, nothing is held, and the
// thread gets no waiting line.
func TestSessionConnect_CollaboratorIsToldTheOwnerMustConnect(t *testing.T) {
	fake := newFakeSlackAPI()
	fakeURL := fake.server(t).URL
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok", Done: true}}}
	obo := perUserOBO{tokens: map[string]string{"U001": "tok-initiator", "U002": "tok-collab"}}
	_, srv := newEventsAdapter(t, gw, fakeURL, channelMode, func(a *slackadapter.Adapter) {
		a.OBO = obo
		a.ConnectorPrompts = true
	})

	gw.openConversation(t, "C1", "100.000", "U001", "test-agent")
	sendEvent(t, srv, mention("U001", "start", "100.000", ""))
	require.Eventually(t, func() bool { return gw.dispatchCount() == 1 },
		flowWait, 50*time.Millisecond, "the initiator's turn runs")
	sendEvent(t, srv, mention("U002", "what broke?", "200.000", "100.000"))
	fake.waitForPath(t, "chat.postEphemeral", 1)
	require.Contains(t, allText(fake.pathCalls("chat.postEphemeral")), "wants to join this thread")

	gw.mu.Lock()
	gw.dispatchErr = sessionConnectChallenge()
	gw.mu.Unlock()
	sendAccessInteraction(t, srv, "U001", accessAllowAction, "100.000", "U002", fakeURL+"/response")
	require.Eventually(t, func() bool {
		return strings.Contains(allText(fake.pathCalls("chat.postEphemeral")), "<@U001> needs to sign in once")
	}, flowWait, 50*time.Millisecond, "the collaborator is told the owner must connect")
	for _, c := range fake.pathCalls("chat.postEphemeral") {
		if strings.Contains(allText([]recordedCall{c}), "needs to sign in") {
			require.Equal(t, "U002", c.params["user"], "the note goes to the collaborator")
		}
	}
	_, _, ok := connectButton(fake)
	require.False(t, ok, "the owner's link is not offered to the collaborator")
	_, _, _, ok = connectButtonIn(fake.pathCalls("chat.postMessage"))
	require.False(t, ok, "and goes to no DM")
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), "Waiting for", "no line for a consent that is not the sender's")
	require.NotContains(t, allText(fake.pathCalls("chat.postMessage")), "The turn failed")
}

// Not now on a Session start's Connect prompt declines the sign-in to muster
// as the person, drops the held message, and rewrites the ephemeral, the
// line and the DM card; the Sign in button then continues nothing.
func TestSessionConnect_NotNowDeclinesAndDropsTheHeldMessage(t *testing.T) {
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok"}, {Done: true}}}
	gw.dispatchErr = sessionConnectChallenge()
	accepter := &fakeAccepter{}
	fake, srv, logs := sessionConnectAdapter(t, gw, accepter)
	responseSrv, capturedBody := newResponseURLCapture(t)

	gw.openConversation(t, "C1", "141.000", "U1", "test-agent")
	sendEvent(t, srv, mention("U1", "how many nodes in gazelle?", "141.000", ""))

	var stateID string
	require.Eventually(t, func() bool {
		var ok bool
		_, stateID, ok = connectButton(fake)
		return ok
	}, flowWait, 20*time.Millisecond, "the ephemeral Connect prompt is posted")
	require.Contains(t, fmt.Sprint(fake.pathCalls("chat.postEphemeral")[0].params["blocks"]), "action_id:connector_decline", "the prompt offers Not now")

	sendConnectorInteractionURL(t, srv, "connector_decline", stateID, responseSrv.URL)
	require.Eventually(t, func() bool { return accepter.declines() == 1 }, flowWait, 20*time.Millisecond, "Not now declines the sign-in")
	accepter.mu.Lock()
	require.Equal(t, sessionConnectLink, accepter.declined[0].URL)
	require.Equal(t, "tok", accepter.bearers[0], "as the person")
	accepter.mu.Unlock()
	require.Eventually(t, func() bool {
		return strings.Contains(capturedBody(), "Not signed in.") &&
			strings.Contains(allText(updatesIn(fake, "C1")), "<@U1> did not sign in.") &&
			strings.Contains(allText(updatesIn(fake, "U1")), "Not signed in.")
	}, flowWait, 20*time.Millisecond, "the ephemeral, the line and the DM card say so")
	require.NotEmpty(t, logLine(logs, "record=connector_declined", "user=U1"))

	gw.mu.Lock()
	gw.dispatchErr = nil
	gw.mu.Unlock()
	sendConnectorInteractionURL(t, srv, "connector_connect", stateID, responseSrv.URL)
	time.Sleep(100 * time.Millisecond)
	require.Zero(t, accepter.count(), "the declined prompt's Sign in accepts nothing")
	require.Equal(t, 1, gw.dispatchCount(), "and the held message never runs")
}

// An accept that fails says nothing of the sign-in: the surfaces do not claim
// it, and the replayed start, still asked for a sign-in, prompts again.
func TestSessionConnect_FailedAcceptDoesNotClaimTheSignIn(t *testing.T) {
	gw := &stubGateway{deltas: []channels.OutboundDelta{{Content: "ok"}, {Done: true}}}
	gw.dispatchErr = sessionConnectChallenge()
	accepter := &fakeAccepter{err: errors.New("muster unreachable")}
	fake, srv, _ := sessionConnectAdapter(t, gw, accepter)

	gw.openConversation(t, "C1", "180.000", "U1", "test-agent")
	sendEvent(t, srv, mention("U1", "how many nodes in gazelle?", "180.000", ""))
	var stateID string
	require.Eventually(t, func() bool {
		var ok bool
		_, stateID, ok = connectButton(fake)
		return ok
	}, flowWait, 20*time.Millisecond, "the prompt is posted")

	sendConnectorInteractionURL(t, srv, "connector_connect", stateID, "")
	require.Eventually(t, func() bool { return gw.dispatchCount() == 2 }, flowWait, 20*time.Millisecond, "the held message is replayed")
	require.Eventually(t, func() bool {
		_, refreshed, _, ok := connectButtonIn(updatesIn(fake, "U1"))
		return ok && refreshed != stateID
	}, flowWait, 20*time.Millisecond, "the replayed start refreshes the DM card with a fresh prompt")
	require.NotContains(t, allText(updatesIn(fake, "U1")), "Signed in", "the card claims no sign-in")
	require.NotContains(t, allText(updatesIn(fake, "C1")), "signed in", "nor does the thread's line")
}
