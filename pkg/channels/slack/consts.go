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
)

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
