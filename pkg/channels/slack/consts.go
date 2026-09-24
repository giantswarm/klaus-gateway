package slack

import (
	"fmt"
	"time"

	"github.com/giantswarm/klaus-gateway/pkg/auth/musterlink"
	"github.com/giantswarm/klaus-gateway/pkg/channels"
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
	// evtAgentSessionStopped is the user clicking the stop button Slack renders
	// on the native working indicator. Slack only offers that button to apps
	// subscribed to this event, so the subscription is what creates the button.
	evtAgentSessionStopped = "agent_session_stopped"
)

// tabMessages is the app_home_opened tab value for the assistant Messages tab;
// the home and about tabs are not surfaces the adapter serves.
const tabMessages = "messages"

// subtypeThreadBroadcast marks a thread reply the author asked Slack to also
// send to the channel. It is a normal human reply (its payload carries user,
// text, ts, and thread_ts); every subtype other than it and subtypeFileShare
// (message_changed, message_deleted, bot_message, …) is not a new user
// instruction and is never routed.
const subtypeThreadBroadcast = "thread_broadcast"

// subtypeFileShare marks a message carrying an uploaded file. Slack sets it on
// every human message with an attachment, so it must route like a plain
// message: without it, an image sent as a bare thread reply or a DM (no
// app_mention twin to carry the files) never reaches the agent.
const subtypeFileShare = "file_share"

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
// action. The completed link is confirmed by OnUserLinked from the recorded
// anchor, not by the click.
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

// connectorDismissedNotice replaces a Connect prompt after "Not now": the
// prompt stays away for connectorPromptCooldown.
func connectorDismissedNotice() string {
	return fmt.Sprintf("Not asked again for %s.", spellDuration(connectorPromptCooldown))
}

// connectorResumeText is the synthetic message dispatched into the thread
// after a connector sign-in completes, so the agent retries the blocked tools
// without the user retyping; %s is the backend name.
const connectorResumeText = "I've signed in to %s, continue"

// payloadTypeBlockActions is the interaction payload type for Block Kit button
// clicks; payloadTypeMessageAction is the type for message shortcuts (the ⋯ →
// Apps menu); payloadTypeViewSubmission is the type for a submitted modal (the
// agent picker). Other payload types (global shortcuts) are not routed.
const (
	payloadTypeBlockActions   = "block_actions"
	payloadTypeMessageAction  = "message_action"
	payloadTypeViewSubmission = "view_submission"
)

// Agent picker modal (slashcmd.go): callback and block/action ids, labels,
// and the notices its two steps post through the slash command's response_url.
const (
	askAgentCallbackID       = "ask_agent"
	askAgentAgentBlockID     = "ask_agent_agent"
	askAgentAgentActionID    = "agent"
	askAgentQuestionBlockID  = "ask_agent_question"
	askAgentQuestionActionID = "question"
	askAgentContextBlockID   = "ask_agent_context"
	askAgentContextActionID  = "context"
	askAgentContextValue     = "include"

	askAgentModalTitle          = "New conversation" // modal titles and button labels are capped at 24 chars
	askAgentSubmitLabel         = "Start thread"
	askAgentShortcutSubmitLabel = "Start conversation" // the shortcut's thread exists already
	askAgentCloseLabel          = "Cancel"
	askAgentAgentLabel          = "Agent"
	askAgentAgentPlaceholder    = "Pick an agent"
	askAgentQuestionLabel       = "Prompt"
	askAgentQuestionPlaceholder = "What should the agent look into?"
	// askAgentDefaultHint sits under the agent select when the default agent
	// is on the list; %s is its display name.
	askAgentDefaultHint = "%s is the default for this workspace."
	// The line that opens the modal: where the conversation lands. %s is the
	// channel, rendered by Slack from its <#id> mention. In a channel the line
	// ends with askAgentLeadAudience; a DM has no other readers and no one else
	// to instruct the agent, so its line says less.
	askAgentLeadNewThread     = "Starts a thread in <#%s> under the agent's name."
	askAgentLeadMessageThread = "Starts this message's thread in <#%s> under the agent's name."
	askAgentLeadThread        = "Continues this thread in <#%s> under the agent's name."
	askAgentLeadAudience      = " Anyone in the channel can read it; you decide who may instruct the agent."
	askAgentLeadDMMessage     = "Starts this message's thread under the agent's name."
	askAgentLeadDM            = "Continues this thread under the agent's name."
	// askAgentContextLabel titles the thread-context checkbox and
	// askAgentContextOption is its one option. Deliberately without a count:
	// counting the thread would mean reading it before views.open, and the
	// trigger the modal opens on dies three seconds after Slack issued it.
	askAgentContextLabel  = "Thread context"
	askAgentContextOption = "Include the earlier messages in this thread"

	// modalMaxAgents is Slack's static_select option cap; modalOptionLabelMax
	// its option label cap; modalQuestionMax the plain_text_input max_length.
	modalMaxAgents      = 100
	modalOptionLabelMax = 75
	modalQuestionMax    = 3000
	modalHintMax        = 2000 // an input block's hint text

	// askAgentAskedBy is the context line under the question the gateway posts
	// on submit. The agent is the message's author, so it is not repeated.
	askAgentAskedBy = "Asked by <@%s>"

	slashCommandDMNotice         = "This command starts a conversation in a channel. In a direct message, type your question."
	slashCommandSignInNotice     = rosterSignInLead + " in a channel, then run the command again."
	slashCommandSlowNotice       = "Listing the agents took too long for Slack's picker. Run the command again."
	slashCommandOpenFailedNotice = "The agent picker did not open. Run the command again."
	askAgentIncompleteNotice     = "Pick an agent and type a prompt, then submit again."
	// askAgentThreadBoundNotice refuses the shortcut in a thread that already
	// talks to an agent: the picker opens conversations, and a second one in
	// the same thread would fork it. %s is the bound agent's display name.
	askAgentThreadBoundNotice = "This thread already talks to *%s*. Reply in the thread to ask it: the picker starts conversations only in threads without one."
	// askAgentThreadOwnedNotice refuses the shortcut in a thread that already
	// has an owner other than the invoker: the picker would make the owner's
	// delegated identity act on the invoker's word without the owner's consent.
	// A reply in the thread takes the normal path, where the owner is asked.
	// %s is the owner's Slack user id.
	askAgentThreadOwnedNotice = "This thread belongs to <@%s>. Reply in the thread, and they are asked to allow you, or start a new thread."
	askAgentInviteNotice      = "The bot is not a member of this channel, so the conversation did not start. Invite the bot to the channel and try again."
	askAgentPostFailedNotice  = "Your question was not posted in this channel. Try again."
	// threadContextFailedNotice tells the person who opened the conversation
	// that the thread could not be read, so they know the agent is answering
	// without what the thread already said. %s is Slack's reason. The turn
	// itself runs regardless, which is why this is a notice and not a refusal.
	threadContextFailedNotice = "The earlier messages in this thread could not be read (`%s`), so the agent sees only your question."
)

// pickerOpenBudget bounds the work between a slash command arriving and
// views.open: Slack invalidates the trigger_id after 3 seconds.
const pickerOpenBudget = 2500 * time.Millisecond

// inspectShortcutCallbackID is the callback_id of the "Inspect agent steps"
// message shortcut registered in deploy/slack/manifest.yaml. Invoked from any
// message in a thread, it replies with an ephemeral rendering of the thread's
// retained tool-call log (see inspect.go).
const inspectShortcutCallbackID = "inspect_agent_steps"

// askAgentShortcutCallbackID is the callback_id of the "Ask an agent here"
// message shortcut registered in deploy/slack/manifest.yaml. Invoked on any
// message, it opens the agent picker and starts the conversation inside that
// message's thread — the one thing the slash command cannot do, since Slack
// sends no thread with it (see slashcmd.go).
const askAgentShortcutCallbackID = "ask_agent_here"

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
const thinkingPlaceholder = "Working…"

// busyNotice is posted when a turn is rejected because another turn is already
// in flight on the same thread (per-thread serialization).
const busyNotice = "Still answering the previous message. Send this one again once the reply has landed, or reply `stop` to interrupt it."

// tokenErrorNotice is shown (ephemerally) when minting a user's muster token
// fails for a reason other than not being linked (a transient refresh failure).
// storeUnavailableNotice tells the author of a message that the routing store
// could not record the thread, so the turn was not run. Ephemeral, in-thread.
const storeUnavailableNotice = "The thread state could not be read, so your message was not sent to the agent. Try again in a moment."

const tokenErrorNotice = "Your Giant Swarm sign-in could not be refreshed. Try again in a moment. If it keeps failing, mention the bot with `/login` to sign in again."

// logoutFailedNotice is shown (ephemerally) when /logout could not remove the
// link from the store, so the person does not believe they are signed out.
const logoutFailedNotice = "The sign-out failed: your sign-in could not be removed. Mention the bot with `/logout` again in a moment."

// accessDecisionRefusal is shown (ephemerally) when a user who is not permitted
// in the thread clicks an in-thread tool Approve/Deny button.
const accessDecisionRefusal = "Only the thread owner and the people they allowed can approve or deny this action."

// accessPromptExpiredNotice replaces an access-consent prompt whose thread
// state this process no longer holds (restart or TTL sweep), so the clicker is
// not left with a button that silently does nothing.
const accessPromptExpiredNotice = "This request expired because the thread state was lost. Ask <@%s> to send their message again."

// accessDeniedNewcomerNotice is shown (ephemerally) to a parked newcomer when
// the thread owner declines them, closing the loop opened by the waiting ack.
const accessDeniedNewcomerNotice = "The thread owner declined, so the agent does not act on your messages in this thread. Mention the bot in a new thread to start your own."

// parkedDropNotice is shown (ephemerally) to a user whose parked messages
// overflowed the per-thread cap, so the drop is visible and the user knows to
// resend. %d is maxParkedPerThread.
const parkedDropNotice = "Only your last %d messages are held here; earlier ones were dropped. Send them again when the agent can act on your messages."

// parkedDropNoticeTTL bounds how often the parked-drop notice repeats per
// (user, thread), so a long burst past the cap nudges once instead of once per
// dropped message.
const parkedDropNoticeTTL = time.Hour

// stopNothingRunningNotice replies to a /stop in a thread with no in-flight
// turn and no pending prompt, instead of falsely confirming a stop.
const stopNothingRunningNotice = "Nothing is running in this thread."

// stopStoppedNotice confirms a turn interrupted by the /stop command. The
// command's own message is in the thread above it, so the thread can already
// see who asked.
const stopStoppedNotice = "Stopped."

// stopStoppedByNotice confirms a turn interrupted with the native stop button.
// %s is the presser's Slack user ID. The press leaves no message of its own, so
// unlike /stop this notice is the thread's only record of who stopped the turn.
const stopStoppedByNotice = "Stopped by <@%s>"

// notPermittedNotice refuses a caller who may read the thread but was never let
// in to instruct the agent there. Shared by the gated commands and the native
// stop button, which enforce the same per-thread rule.
const notPermittedNotice = "You can read this thread, but only the people the thread owner allowed can instruct the agent, and that includes commands. Post a message, and the owner can let you in."

// signInLinkExpiredNote replaces a sign-in prompt whose link outlived its
// state TTL once a fresh prompt is posted, so the dead button cannot be
// mistaken for the live one. Only a DM prompt is rewritten this way; a channel
// prompt is ephemeral and has no addressable ts.
const signInLinkExpiredNote = "This sign-in link expired. Use the newer one below."

// signInLinkSupersededNote leads a channel sign-in prompt that replaces one
// whose link expired. Slack cannot rewrite or delete an ephemeral, so the dead
// button stays on the user's screen until their client reloads; the fresh
// prompt carries the warning that a DM's predecessor is rewritten to carry.
const signInLinkSupersededNote = "The earlier sign-in link expired. Use this one."

// The sign-in card: its title, its body (%d is the link's lifetime in
// minutes), the line each trigger adds, and the context line under the button.
// The card names no one: it reaches its user through the message's audience
// (an ephemeral in a channel, a DM thread otherwise).
const (
	signInPromptTitle      = "Sign in to Giant Swarm"
	signInPromptBodyFormat = "The agent runs its tools with your own permissions, so it needs your sign-in once. The link is valid for %d minutes."
	signInForMessageLine   = "Your message runs as soon as you sign in."
	signInForClickLine     = "Sign in, then click the button again."
	signInSessionHint      = "Signed in before? Your session may have expired."
)

// The thread notice anchors a channel thread's ephemeral sign-in prompts:
// Slack does not surface a thread-scoped ephemeral in a thread with no
// messages (klaus-gateway#156). It names who the thread is waiting for, and
// after the last of them signs in, who signed in. It never carries the link,
// which is minted for one identity and stays in the ephemeral
// (klaus-gateway#185). %s is the people, as mentions.
const (
	signInWaitingFormat  = "Waiting for %s to sign in to Giant Swarm"
	signInSignedInFormat = "%s signed in to Giant Swarm"
)

// oboDisabledNotice and noSlackUserNotice answer /login and /logout when
// sign-in cannot run at all.
const (
	oboDisabledNotice = "Sign-in is not enabled on this gateway."
	noSlackUserNotice = "Your Slack user could not be read, so sign-in is not available."
)

// The /login and /logout replies. They are ephemeral: the first carries the
// caller's email, which a shared thread must not see.
const (
	loginSignedInAsNotice = "Signed in as %s."
	loginSignedInNotice   = "Signed in."
	logoutNotice          = "Signed out. The agent asks you to sign in again before it acts for you."
)

// signedInNotice confirms a completed account link. It names no identity: the
// email the user signed in as is shown on the private browser success page.
const signedInNotice = "Signed in to Giant Swarm. The agent now acts with your permissions."

// signInNudgeTTL bounds how long a posted sign-in prompt suppresses a fresh
// nudge for the same (user, thread). It is the sign-in link's state lifetime:
// past it the posted button's URL is dead, so re-prompting with a fresh URL
// beats staying silent behind it.
const signInNudgeTTL = musterlink.DefaultStateTTL

// choiceSelectNudge is shown (ephemerally) when a user clicks Submit on an
// ask_user choice widget without selecting anything; the task stays pending.
const choiceSelectNudge = "Pick at least one option, then click Submit."

// promptSupersededNotice replaces a prompt message whose button was clicked
// after its task was already resumed and the thread paused on a newer prompt,
// so the click cannot deliver answers the user never saw.
const promptSupersededNotice = "A newer prompt replaced this one. Answer the latest one in this thread."

// promptAnsweredNotice replaces a prompt whose task is no longer pending:
// answered, dropped by the TTL, or lost in a restart.
const promptAnsweredNotice = "Already answered."

// formIncompleteNudge is shown (ephemerally) when a user clicks Submit on a
// multi-question ask_user form with a question still unanswered; the form stays
// pending so the user can complete it and submit again.
const formIncompleteNudge = "Answer every question, then click Submit."

// The line that replaces a question prompt's controls once it is answered:
// the answer (a single question) or nothing (a form, whose answers sit under
// their questions), who answered, and a Slack date token for when.
const (
	questionAnsweredFormat = "%s · answered by <@%s> · %s"
	formAnsweredFormat     = "Answered by <@%s> · %s"
	// formNoAnswer stands under a form question a typed reply left without an
	// answer (one line per question), so the thread sees what the agent got.
	formNoAnswer = "No answer"
)

// The approval card: its title, the context line naming who may decide, the
// button labels, and the line that replaces the buttons after a decision. The
// decision lines end with a Slack date token, so each reader sees the time in
// their own time zone. The card has no "Ask a question" button: the Go ADK
// runtime drops a rejection's reason, so the model never sees the question
// (giantswarm/kagent-upstream#71).
const (
	approvalRequiredTitle = "Approval required"
	approvalDeciders      = "<@%s> or the people they allowed can decide"
	approvalApproveLabel  = "Approve"
	approvalDenyLabel     = "Deny"
	approvalApprovedBy    = "Approved by <@%s> · %s"
	approvalDeniedBy      = "Denied by <@%s> · %s"
)

// emptyOutputNote replaces the text-mode placeholder when a turn completes
// without producing any output, so it does not linger as "thinking".
const emptyOutputNote = "The agent finished without a reply."

// stoppedNote replaces the text-mode placeholder when a turn is cancelled
// before any content streamed, so "thinking" does not linger under "Stopped.".
const stoppedNote = "Stopped before an answer."

// pausedNote replaces the text-mode placeholder when a turn pauses on an
// input-required prompt before any content streamed.
const pausedNote = "Waiting for your answer below."

// failedNote is posted when a turn ends in error before any answer text: it
// replaces the text-mode placeholder (so it does not linger as "thinking"),
// and in reactions mode it is posted into the thread next to the failed emoji,
// which alone would leave the user guessing whether a retry helps.
const failedNote = "The turn failed before an answer. Send the message again to retry."

// The notes of a turn that failed on something the gateway can name
// (channels.ClassifyFailure). They replace failedNote because they say what
// broke and whether trying again helps: a tools or platform failure was
// already retried once and is not the person's to fix, a model error usually
// passes, a policy refusal stays.
const (
	toolsFailedNote    = "The agent could not connect to its tools, so it did not work on your message. The problem is on the platform side, not in your message, and a retry right now does not help."
	platformFailedNote = "The agent platform could not be reached, so the agent did not work on your message. The problem is on the platform side, not in your message, and a retry right now does not help."
	modelFailedNote    = "The model behind this agent returned an error instead of an answer. This is usually temporary: try again in a minute."
	policyFailedNote   = "A platform policy refused this request, so the agent did not answer it. Sending it again does not change that."
)

// failureNote is the note of a turn that failed with err before the agent
// answered: the class's own note, or failedNote when no class names it.
func failureNote(err error) string {
	switch channels.ClassifyFailure(err) {
	case channels.FailureTools:
		return toolsFailedNote
	case channels.FailurePlatform:
		return platformFailedNote
	case channels.FailureModel:
		return modelFailedNote
	case channels.FailurePolicy:
		return policyFailedNote
	}
	return failedNote
}

// renderFailedNote is posted when the agent completed its turn but Slack kept
// refusing the reply's final rendering, so the thread knows the text above is
// incomplete rather than the whole answer. It names Slack's error code when
// there is one.
func renderFailedNote(err error) string {
	reason := apiErrorCode(err)
	if reason == "" {
		reason = err.Error()
	}
	return fmt.Sprintf("The agent finished, but Slack refused the rest of the reply: %s.", escapeMrkdwn(reason))
}

// attachmentsUnavailableNote is posted when a message carried only attachments
// and none of them could be downloaded, so there is nothing to send the agent.
const attachmentsUnavailableNote = "Your attachments could not be downloaded, so nothing was sent to the agent. Try again."

// hitlTextReplyNeededNote is posted when a reply into a thread with a paused
// confirmation carries no text (attachment only): no decision can be read from
// an empty reply, so the task stays pending and the user is asked to answer in
// words.
const hitlTextReplyNeededNote = "This thread waits for the confirmation above, and a file alone is not a decision. Reply with text, for example `approve` or `deny`. The file was not sent to the agent."

// payloadTooLargeNote is posted when the agent rejects a turn as too large and
// the message carried no attachments, so the size is the text/history rather
// than a file the user can shrink.
const payloadTooLargeNote = "That was too large for the agent to accept, so it was not sent. Try a shorter message, or start a new thread."

// corruptSessionResetNotice is posted after a corrupt-history failure when the
// broken kagent session was deleted, so the user knows to resend rather than
// retry into the same failure.
const corruptSessionResetNotice = "An earlier interrupted turn corrupted this conversation's history, and the agent could no longer read it. The session is reset: send your message again to continue. The earlier context of this thread is lost."

// corruptSessionStuckNotice is posted after a corrupt-history failure when the
// session could not be deleted; the thread cannot recover.
const corruptSessionStuckNotice = "An earlier interrupted turn corrupted this conversation's history, and the session could not be reset. Start a new thread."

// resumeStartingFreshNotice is posted when a reply lands in a thread whose
// kagent session no longer exists, so the user is not confused by lost context.
const resumeStartingFreshNotice = "The earlier conversation in this thread was not found, so the agent starts fresh."

// threadClosedNotice tells the author of a reply in a thread whose
// conversation ended — no message for the whole thread lifetime — why nobody
// answers, and how to start again. Ephemeral: the rest of the thread does not
// need it.
func threadClosedNotice(lifetime time.Duration) string {
	return fmt.Sprintf("This conversation ended after %s without messages. Mention the bot to start a new one.", spellDuration(lifetime))
}

// spellDuration writes a thread lifetime the way the notice reads it, and
// never shorter than it is: the count of the largest unit the duration is a
// whole multiple of. 90 days is "90 days", 36 hours is "36 hours" (not "1
// day"), 90 minutes is "90 minutes" (not "1 hour"). A duration that is no
// whole number of seconds is written as Go writes it ("1.5s"): exact, and
// nobody configures a thread lifetime like that.
func spellDuration(d time.Duration) string {
	for _, u := range []struct {
		size time.Duration
		name string
	}{
		{24 * time.Hour, "day"},
		{time.Hour, "hour"},
		{time.Minute, "minute"},
		{time.Second, "second"},
	} {
		if d >= u.size && d%u.size == 0 {
			return countOf(int64(d/u.size), u.name)
		}
	}
	return d.String()
}

func countOf(n int64, unit string) string {
	if n == 1 {
		return "1 " + unit
	}
	return fmt.Sprintf("%d %ss", n, unit)
}

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
const dmRedirect = "Swarmgeist works in channels, not in direct messages. Invite it to a channel and mention `@Swarmgeist` there to start."

// channelNotServed tells a user the channel is outside the configured
// allowlist: ephemerally on a mention, through the response_url on a slash
// command.
const channelNotServed = "This channel is not enabled yet. Ask a platform admin to add it to the channel allowlist."

// Slack Web API parameter keys (form-encoded and JSON body).
const (
	paramChannel   = "channel"
	paramText      = "text"
	paramTS        = "ts"
	paramThreadTS  = "thread_ts"
	paramUser      = "user"
	paramBlocks    = "blocks"
	paramTimestamp = "timestamp"  // reactions.* target message ts
	paramName      = "name"       // reactions.* emoji name
	paramUsername  = "username"   // chat:write.customize display name
	paramIconURL   = "icon_url"   // chat:write.customize display icon
	paramChannelID = "channel_id" // agents.sessions.setStatus channel
	paramStatus    = "status"     // agents.sessions.setStatus lifecycle state
	paramTitle     = "title"      // agents.sessions.setStatus session name (create only)
	// agents.sessions.setStatus session starter (create only)
	paramInitiatorUserID = "initiator_user_id"

	// Streamed reply parameters (chat.startStream / appendStream / stopStream).
	// chunks carries everything the reply adds — the agent's prose as
	// markdown_text chunks, its tool steps as task_update chunks — in one
	// ordered array. It is the alternative to the plain markdown_text field,
	// and the two may not be combined; a message uses one of them from its
	// first call to its last.
	paramChunks = "chunks"
	// recipient_user_id and recipient_team_id name the person the streamed
	// answer is for; Slack requires both when the stream is in a channel and
	// refuses them in a DM.
	paramRecipientUserID = "recipient_user_id"
	paramRecipientTeamID = "recipient_team_id"
	// session_status is the agent session state chat.stopStream leaves the
	// thread in. Slack defaults it to active, so it is always sent explicitly.
	paramSessionStatus = "session_status"
	// unfurl_links / unfurl_media are forced to false on every chat.postMessage:
	// bot posts relay agent- and tool-controlled links, and an unfurl has
	// Slack's crawler fetch them (fatal for single-use auth links).
	paramUnfurlLinks = "unfurl_links"
	paramUnfurlMedia = "unfurl_media"

	paramTriggerID = "trigger_id" // views.open
	paramView      = "view"       // views.open

	paramLimit  = "limit"  // conversations.replies page size
	paramCursor = "cursor" // conversations.replies paging cursor
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

	// Modal / input block keys (the agent picker).
	bkCallbackID      = "callback_id"
	bkPrivateMetadata = "private_metadata"
	bkTitle           = "title"
	bkSubmit          = "submit"
	bkClose           = "close"
	bkBlocks          = "blocks"
	bkLabel           = "label"
	bkElement         = "element"
	bkPlaceholder     = "placeholder"
	bkInitialOption   = "initial_option"
	bkInitialOptions  = "initial_options"
	bkOptional        = "optional"
	bkInitialValue    = "initial_value"
	bkMultiline       = "multiline"
	bkMaxLength       = "max_length"
	bkHint            = "hint"
)

// Block Kit type values.
const (
	bkSection        = "section"
	bkContext        = "context" // small muted text; carries the tool-activity entries
	bkHeader         = "header"
	bkDivider        = "divider"
	bkActions        = "actions"
	bkButton         = "button"
	bkRadioButtons   = "radio_buttons"
	bkCheckboxes     = "checkboxes"
	bkModal          = "modal"
	bkInput          = "input"
	bkStaticSelect   = "static_select"
	bkPlainTextInput = "plain_text_input"
	bkMrkdwn         = "mrkdwn"
	bkMarkdown       = "markdown" // top-level Slack markdown block
	bkPlainText      = "plain_text"
	bkPrimary        = "primary"
	bkDanger         = "danger"
)
