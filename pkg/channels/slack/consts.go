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

// labelApproved is the human-readable resume text / approve keyword shared by
// the button, free-text, and auto-approve decision paths.
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
