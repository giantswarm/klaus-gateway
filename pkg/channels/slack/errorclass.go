package slack

import "slices"

// errorClass is what a failed Slack Web API call is worth to its caller
// (slackErrorClass); klaus_gateway_slack_call_total counts every call under it.
type errorClass string

const (
	// classPermanent: Slack answers the same call the same way however often
	// it is sent, so the caller gives up at once.
	classPermanent errorClass = "permanent"
	// classRetryable: another attempt may succeed. A network error, a 5xx, a
	// 429 once its Retry-After budget is spent, and every code the tables
	// below do not name.
	classRetryable errorClass = "retryable"
	// classConverged: Slack refused the call because what it asks for is
	// already so; the call counts as done.
	classConverged errorClass = "converged"
)

// permanentSlackErrors are the codes no retry of the same call fixes:
// humanlayer/effect-channels' permanentSlackErrors. A streamed reply that
// outgrew its message moves on to a new one before its msg_too_long is
// classified (overflowStream), and a branded post refused for its display
// identity is sent again unbranded first (identityRejectedErr).
var permanentSlackErrors = []string{
	// The bot token itself is refused (effect-channels' rejectedTokenErrors).
	"account_inactive",
	"invalid_auth",
	"not_authed",
	"token_expired",
	"token_revoked",
	// The token may not do this.
	"missing_scope",
	"not_allowed_token_type",
	"team_access_not_granted",
	// The channel or the thread is gone, or the bot may not post there.
	"channel_not_found",
	"invalid_thread_ts",
	"is_archived",
	"not_in_channel",
	"restricted_action",
	"restricted_action_non_threadable_channel",
	"restricted_action_read_only_channel",
	"restricted_action_thread_locked",
	"restricted_action_thread_only_channel",
	"thread_locked",
	// The message, or the change to it, is refused.
	"as_user_not_supported",
	"cant_delete_message",
	"cant_update_message",
	"compliance_exports_prevent_deletion",
	"edit_window_closed",
	"invalid_blocks",
	errCodeMessageNotFound,
	"msg_too_long",
	"no_text",
}

// errCodeMessageNotFound is permanent, except where the call removes the
// message or something on it (convergedSlackErrors).
const errCodeMessageNotFound = "message_not_found"

// convergedSlackErrors are, per method, the codes that say the call's effect
// is already in place: the reaction is there, or gone, or so is its message,
// or the message to delete.
var convergedSlackErrors = map[string][]string{
	methodReactionsAdd:    {"already_reacted"},
	methodReactionsRemove: {"no_reaction", errCodeMessageNotFound},
	methodChatDelete:      {errCodeMessageNotFound},
}

// slackErrorClass classifies the error a call to method failed with. Only an
// answer Slack gave (apiError) can be permanent or converged: an error that
// carries no Slack code never reached a verdict.
func slackErrorClass(method string, err error) errorClass {
	code := apiErrorCode(err)
	switch {
	case code == "":
		return classRetryable
	case slices.Contains(convergedSlackErrors[method], code):
		return classConverged
	case slices.Contains(permanentSlackErrors, code):
		return classPermanent
	}
	return classRetryable
}
