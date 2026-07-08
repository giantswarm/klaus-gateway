package slack

// Slack event type strings.
const (
	evtMessage    = "message"
	evtAppMention = "app_mention"
)

// HITL Block Kit action IDs.
const (
	hitlApprove = "hitl_approve"
	hitlDeny    = "hitl_deny"
	hitlChoice  = "hitl_choice" // ask_user single-select choice button
)

// Access-consent Block Kit action IDs. The button value encodes the thread and
// the newcomer being decided (see encodeAccessValue), since one initiator can
// have several pending approvals at once.
const (
	accessAllow = "access_allow"
	accessDeny  = "access_deny"
)

// oboSignIn is the action_id on the OBO "Sign in" URL button. The button opens
// its url directly; the interaction payload Slack still sends is ignored
// (classifyAction returns false), so no decision is routed.
const oboSignIn = "obo_sign_in"

// labelApproved is the human-readable resume text / approve keyword shared by
// the button and free-text decision paths.
const labelApproved = "approved"

// wordYes is the plain-affirmative approve keyword.
const wordYes = "yes"

// maxChoiceButtons caps how many ask_user choices are rendered as buttons.
// Beyond this (or for multi-select / multi-question prompts) the choices are
// rendered as text and the user replies free-text in-thread.
const maxChoiceButtons = 5

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

// emptyOutputNote replaces the text-mode placeholder when a turn completes
// without producing any output, so it does not linger as "thinking".
const emptyOutputNote = "_(the agent finished without a reply)_"

// failedNote replaces the text-mode placeholder when a turn ends in error, so it
// does not linger as "thinking" with no failure signal (reactions mode swaps in
// the failed emoji instead).
const failedNote = "_(the turn failed; please try again)_"

// resumeStartingFreshNotice is posted when a reply lands in a thread whose
// kagent session no longer exists, so the user is not confused by lost context.
const resumeStartingFreshNotice = "I couldn't find our earlier conversation in this thread, so I'm starting fresh."

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
)

// bkURL is the Block Kit button "url" field (opens a link on click).
const bkURL = "url"

// Block Kit JSON field keys.
const (
	bkType     = "type"
	bkText     = "text"
	bkActionID = "action_id"
	bkValue    = "value"
	bkStyle    = "style"
	bkElements = "elements"
)

// Block Kit type values.
const (
	bkSection   = "section"
	bkActions   = "actions"
	bkButton    = "button"
	bkMrkdwn    = "mrkdwn"
	bkMarkdown  = "markdown" // top-level Slack markdown block
	bkPlainText = "plain_text"
	bkPrimary   = "primary"
	bkDanger    = "danger"
)
