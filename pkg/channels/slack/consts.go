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

// oboSignIn is the action_id on the OBO "Sign in" URL button. The button opens
// its url directly; the interaction payload Slack still sends is ignored
// (classifyAction returns false), so no decision is routed.
const oboSignIn = "obo_sign_in"

// labelApproved is the human-readable resume text / approve keyword shared by
// the button and free-text decision paths.
const labelApproved = "approved"

// maxChoiceButtons caps how many ask_user choices are rendered as buttons.
// Beyond this (or for multi-select / multi-question prompts) the choices are
// rendered as text and the user replies free-text in-thread.
const maxChoiceButtons = 5

// Slack Web API parameter keys (form-encoded and JSON body).
const (
	paramChannel  = "channel"
	paramText     = "text"
	paramTS       = "ts"
	paramThreadTS = "thread_ts"
	paramUser     = "user"
	paramBlocks   = "blocks"
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
	bkPlainText = "plain_text"
	bkPrimary   = "primary"
	bkDanger    = "danger"
)
