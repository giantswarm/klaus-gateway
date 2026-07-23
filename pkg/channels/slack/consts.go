package slack

import (
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/auth/musterlink"
)

// Slack event type strings. Slack's current Agent view no longer sends
// assistant_thread_started; app_home_opened (tab "messages") and
// app_context_changed are its replacement lifecycle events
// (docs.slack.dev/ai/developing-ai-apps).
const (
	evtMessage           = "message"
	evtAppMention        = "app_mention"
	evtMemberJoined      = "member_joined_channel"
	evtAppHomeOpened     = "app_home_opened"
	evtAppContextChanged = "app_context_changed"
)

// tabMessages is the app_home_opened tab value for the assistant Messages tab;
// the home and about tabs are not surfaces the adapter serves.
const tabMessages = "messages"

// subtypeThreadBroadcast marks a thread reply the author asked Slack to also
// send to the channel. It is a normal human reply (its payload carries user,
// text, ts, and thread_ts) and is the only message subtype the adapter routes;
// every other subtype (message_changed, message_deleted, bot_message, …) is
// not a new user instruction.
const subtypeThreadBroadcast = "thread_broadcast"

// HITL Block Kit action IDs.
const (
	hitlApprove = "hitl_approve"
	hitlDeny    = "hitl_deny"
	hitlChat    = "hitl_chat"   // reply with a follow-up question instead of yes/no
	hitlChoice  = "hitl_choice" // ask_user single long-text choice (section accessory button, per index)
	hitlSubmit  = "hitl_submit" // ask_user radio/checkbox Submit button
	hitlGroup   = "hitl_group"  // ask_user radio_buttons/checkboxes element action_id
)

// hitlGroupBlock is the block_id of an ask_user radio/checkbox block. The
// interaction handler reads the selection out of state.values[block_id][action_id]
// on Submit; a per-choice checkbox layout appends "_<index>" for a stable id per block.
const hitlGroupBlock = "hitl_group_block"

// hitlQGroupPrefix prefixes the block_id of one question's widget in a
// multi-question ask_user form: the full id is hitlQGroupPrefix + "_<question
// index>". The handler maps each selection back to its question by parsing the
// index out of the block_id (see choiceSelections). The prefix is
// distinct from hitlGroupBlock so the single-question and form readers never
// cross-read one another's state.
const hitlQGroupPrefix = "hitl_q"

// Access-consent Block Kit action IDs. The button value encodes the thread and
// the newcomer being decided (see encodeAccessValue), since one initiator can
// have several pending approvals at once.
const (
	accessAllow = "access_allow"
	accessDeny  = "access_deny"
)

// oboSignIn is the action_id on the OBO "Sign in" URL button. The button opens
// its url directly; the interaction payload Slack still sends is acked without
// action. The prompt message itself is rewritten in place once the link
// completes (OnUserLinked), keyed by the recorded anchor, not by the click.
const oboSignIn = "obo_sign_in"

// Connector Block Kit action IDs. The button value carries the backend name.
// connectorConnect is a URL button (opens the backend's consent flow in the
// browser); connectorDismiss suppresses the prompt for that backend until the
// cooldown lapses.
const (
	connectorConnect = "connector_connect"
	connectorDismiss = "connector_dismiss"
)

// musterAuthLoginTool is the muster tool whose result carries a backend login
// link; its appearance in the agent's stream drives the Connect prompt.
const musterAuthLoginTool = "core_auth_login"

// musterCallToolMetaTool is muster's aggregating meta-tool. Agents typically
// reach core_auth_login through it, so the inner tool name arrives in the
// call's arguments ({"name": "core_auth_login", ...}) rather than as the
// stream's tool name.
const musterCallToolMetaTool = "call_tool"

// connectorPromptCooldown bounds how often the connect prompt re-posts for one
// (user, backend) while it is neither connected nor dismissed, so an ignored
// prompt does not repeat on every message.
const connectorPromptCooldown = time.Hour

// connectorCheckTimeout bounds the async prompt post; it runs on the adapter
// lifecycle context, off the turn's critical path.
const connectorCheckTimeout = 10 * time.Second

// maxConnectorNameLen bounds the backend name accepted from a button value
// (interaction payloads are attacker-shaped input).
const maxConnectorNameLen = 128

// connectorCompletionTTL bounds a connector completion state: the window
// between the Connect prompt posting and the browser landing back on the
// gateway. Matched to the login link's own lifetime; past it the landing
// renders the expired page and no resume fires.
const connectorCompletionTTL = musterlink.DefaultStateTTL

// connectorResumeText is the synthetic message dispatched into the thread
// after a connector sign-in completes, so the agent retries the blocked tools
// without the user retyping; %s is the backend name.
const connectorResumeText = "I've signed in to %s, continue"

// payloadTypeBlockActions is the interaction payload type for Block Kit button
// clicks; other payload types (view submissions, shortcuts) are not routed.
const payloadTypeBlockActions = "block_actions"

// labelApproved is the human-readable resume text / approve keyword shared by
// the button and free-text decision paths.
const labelApproved = "approved"

// wordYes is the plain-affirmative approve keyword.
const wordYes = "yes"

// maxChoiceOptions caps how many ask_user choices render as an interactive
// widget (radio_buttons/checkboxes cap at 10 options; the section-per-choice
// long-text layout stays under the 50-blocks-per-message limit). Beyond this,
// or for multi-question prompts, choices render as text and the user replies
// free-text in-thread.
const maxChoiceOptions = 10

// choiceLabelWidgetMax is the Block Kit option-object text limit (75 runes for
// select/overflow and radio/checkbox alike). A choice longer than this cannot
// render in a widget without truncation, so the renderer falls back to the
// section-per-choice layout, which carries the full text in a 3000-char section.
const choiceLabelWidgetMax = 75

// maxFormQuestions caps how many questions a multi-question ask_user prompt
// renders as a single interactive form. Each question costs a section plus a
// widget block, and the form adds one Submit (2N+1 blocks), so this keeps a
// full form under the 50-blocks-per-message limit. A prompt with more questions
// renders as text.
const maxFormQuestions = 20

// Progress-mode values (Adapter.ProgressMode).
const (
	progressModeAuto      = "auto"      // reactions, falling back to text on missing_scope
	progressModeReactions = "reactions" // reactions only
	progressModeText      = "text"      // text placeholder only
)

// Default progress reaction emoji names (no surrounding colons). Overridable
// via config so a workspace can pick emoji its members recognise.
const (
	defaultWorkingEmoji = "eyes"
	defaultDoneEmoji    = "white_check_mark"
	defaultFailedEmoji  = "x"
)

// thinkingPlaceholder is the text-mode progress placeholder, posted before the
// first agent output and replaced by the answer.
const thinkingPlaceholder = "_thinking…_"

// busyNotice is posted when a turn is rejected because another turn is already
// in flight on the same thread (per-thread serialization).
const busyNotice = "I'm still finishing your previous message in this thread. Give me a moment and try again once I've replied."

// tokenErrorNotice is shown (ephemerally) when minting a user's muster token
// fails for a reason other than not being linked (a transient refresh failure).
const tokenErrorNotice = "I couldn't refresh your Giant Swarm sign-in just now. Please try again in a moment; if it keeps failing, re-link with the `/login` command."

// accessDecisionRefusal is shown (ephemerally) when a user who is not permitted
// in the thread clicks an in-thread tool Approve/Deny button.
const accessDecisionRefusal = "_Only the thread owner (and people they've allowed) can approve or deny this action._"

// accessPromptExpiredNotice replaces an access-consent prompt whose thread
// state this process no longer holds (restart or TTL sweep), so the clicker is
// not left with a button that silently does nothing.
const accessPromptExpiredNotice = "_This approval expired (I lost the thread state). Ask <@%s> to resend their message._"

// accessDeniedNewcomerNotice is shown (ephemerally) to a parked newcomer when
// the thread owner declines them, closing the loop opened by the waiting ack.
const accessDeniedNewcomerNotice = "_The thread owner declined, so I won't act on your messages in this thread. Mention me in a new thread to start your own._"

// parkedDropNotice is shown (ephemerally) to a user whose parked messages
// overflowed the per-thread cap, so the drop is visible and the user knows to
// resend. %d is maxParkedPerThread.
const parkedDropNotice = "_I can only hold your last %d messages here; earlier ones were dropped, please resend them once I can act on your messages._"

// parkedDropNoticeTTL bounds how often the parked-drop notice repeats per
// (user, thread), so a long burst past the cap nudges once instead of once per
// dropped message.
const parkedDropNoticeTTL = time.Hour

// stopNothingRunningNotice replies to a /stop in a thread with no in-flight
// turn and no pending prompt, instead of falsely confirming a stop.
const stopNothingRunningNotice = "_Nothing is running in this thread._"

// signInLinkExpiredNote replaces a sign-in prompt whose link outlived its
// state TTL once a fresh prompt is posted, so the dead button cannot be
// mistaken for the live one.
const signInLinkExpiredNote = "_This sign-in link expired; use the newer one below._"

// signInNudgeTTL bounds how long a posted sign-in prompt suppresses a fresh
// nudge for the same (user, thread). It is the sign-in link's state lifetime:
// past it the posted button's URL is dead, so re-prompting with a fresh URL
// beats staying silent behind it.
const signInNudgeTTL = musterlink.DefaultStateTTL

// choiceSelectNudge is shown (ephemerally) when a user clicks Submit on an
// ask_user choice widget without selecting anything; the task stays pending.
const choiceSelectNudge = "_Pick at least one option, then click Submit._"

// promptSupersededNotice replaces a prompt message whose button was clicked
// after its task was already resumed and the thread paused on a newer prompt,
// so the click cannot deliver answers the user never saw.
const promptSupersededNotice = "_This prompt was superseded; please answer the latest one in this thread._"

// formIncompleteNudge is shown (ephemerally) when a user clicks Submit on a
// multi-question ask_user form with a question still unanswered; the form stays
// pending so the user can complete it and submit again.
const formIncompleteNudge = "_Please answer every question, then click Submit._"

// chatModePrompt replaces the approval buttons after the user clicks "Chat":
// the pending tool call is held and the next in-thread reply is sent as the
// follow-up question.
const chatModePrompt = "💬 _Ask your question in this thread; I'll answer, then ask you to confirm again._"

// emptyOutputNote replaces the text-mode placeholder when a turn completes
// without producing any output, so it does not linger as "thinking".
const emptyOutputNote = "_(the agent finished without a reply)_"

// stoppedNote replaces the text-mode placeholder when a turn is cancelled
// before any content streamed, so "thinking" does not linger under "Stopped.".
const stoppedNote = "_(stopped)_"

// pausedNote replaces the text-mode placeholder when a turn pauses on an
// input-required prompt before any content streamed.
const pausedNote = "_(waiting for your input below)_"

// failedNote replaces the text-mode placeholder when a turn ends in error, so it
// does not linger as "thinking" with no failure signal (reactions mode swaps in
// the failed emoji instead).
const failedNote = "_(the turn failed; please try again)_"

// corruptSessionResetNotice is posted after a corrupt-history failure when the
// broken kagent session was deleted, so the user knows to resend rather than
// retry into the same failure.
const corruptSessionResetNotice = "An earlier interrupted turn corrupted this conversation's history, and the agent could no longer read it. I've reset the session: please resend your message and we'll continue from a clean slate (earlier context in this thread is lost)."

// corruptSessionStuckNotice is posted after a corrupt-history failure when the
// session could not be deleted; the thread cannot recover.
const corruptSessionStuckNotice = "An earlier interrupted turn corrupted this conversation's history, and I couldn't reset it automatically. Please start a new thread."

// resumeStartingFreshNotice is posted when a reply lands in a thread whose
// kagent session no longer exists, so the user is not confused by lost context.
const resumeStartingFreshNotice = "I couldn't find our earlier conversation in this thread, so I'm starting fresh."

// channelIntro is posted once when the bot is added to a channel, so members
// know what it is and how to reach it.
const channelIntro = "👋 Hi, I'm Swarmgeist. Mention me (`@Swarmgeist`) in this channel to start a thread and I'll bring in an agent to help investigate and act on your clusters. I reply in-thread and ask before anything destructive. Mention me with `/help` (as in `@Swarmgeist /help`) for the full list of commands."

// assistantGreeting is posted into a user's assistant pane the first time they
// open it (app_home_opened, Messages tab), so a new assistant thread does not
// open bare.
const assistantGreeting = "👋 Hi, I'm Swarmgeist. Ask me here about your clusters and platform and I'll bring in an agent to help investigate and act. I ask before anything destructive. Send `/help` for the full list of commands."

// homeGreetingTTL bounds how often the assistant-pane greeting repeats per
// user: app_home_opened fires on every pane open, not once per thread.
const homeGreetingTTL = 24 * time.Hour

// dmRedirect is posted when a user DMs the bot while DMs are in redirect mode,
// pointing them to a channel instead.
const dmRedirect = "I work in channels, not direct messages. Invite me to a channel and mention me there (`@Swarmgeist`) to get started."

// channelNotServed is sent ephemerally when a user mentions the bot in a
// channel outside the configured allowlist.
const channelNotServed = "I'm not enabled in this channel yet. Ask a platform admin to add it to my channel allowlist."

// Slack Web API parameter keys (form-encoded and JSON body).
const (
	paramChannel   = "channel"
	paramText      = "text"
	paramTS        = "ts"
	paramThreadTS  = "thread_ts"
	paramUser      = "user"
	paramBlocks    = "blocks"
	paramTimestamp = "timestamp" // reactions.* target message ts
	paramName      = "name"      // reactions.* emoji name
	paramUsername  = "username"  // chat:write.customize display name
	paramIconURL   = "icon_url"  // chat:write.customize display icon
	// unfurl_links / unfurl_media are forced to false on every chat.postMessage:
	// bot posts relay agent- and tool-controlled links, and an unfurl has
	// Slack's crawler fetch them (fatal for single-use auth links).
	paramUnfurlLinks = "unfurl_links"
	paramUnfurlMedia = "unfurl_media"
)

// bkURL is the Block Kit button "url" field (opens a link on click).
const bkURL = "url"

// Block Kit JSON field keys.
const (
	bkType      = "type"
	bkText      = "text"
	bkActionID  = "action_id"
	bkValue     = "value"
	bkStyle     = "style"
	bkElements  = "elements"
	bkOptions   = "options"
	bkBlockID   = "block_id"
	bkAccessory = "accessory"
)

// Block Kit type values.
const (
	bkSection      = "section"
	bkActions      = "actions"
	bkButton       = "button"
	bkRadioButtons = "radio_buttons"
	bkCheckboxes   = "checkboxes"
	bkMrkdwn       = "mrkdwn"
	bkMarkdown     = "markdown" // top-level Slack markdown block
	bkPlainText    = "plain_text"
	bkPrimary      = "primary"
	bkDanger       = "danger"
)
