package musterlink

import (
	"net/http"

	"github.com/giantswarm/pagekit"
)

// Next steps of the sign-in error pages.
const (
	messageStartAgain = "Return to Slack and start the sign-in again."
	messageTryLater   = "Try again in a moment."
)

// knownErrorCodes are the error codes an authorization response may carry:
// RFC 6749 section 4.1.2.1, OpenID Connect Core section 3.1.2.6 and RFC 8707.
// A page shows the code only when it is one of these, since anyone can send
// a browser to the callback with an error parameter of their choosing.
var knownErrorCodes = map[string]bool{
	"invalid_request":            true,
	"unauthorized_client":        true,
	"access_denied":              true,
	"unsupported_response_type":  true,
	"invalid_scope":              true,
	"server_error":               true,
	"temporarily_unavailable":    true,
	"interaction_required":       true,
	"login_required":             true,
	"account_selection_required": true,
	"consent_required":           true,
	"invalid_request_uri":        true,
	"invalid_request_object":     true,
	"request_not_supported":      true,
	"request_uri_not_supported":  true,
	"registration_not_supported": true,
	"invalid_target":             true,
}

// signedInPage is the landing after a completed Slack sign-in; message says
// what to do next.
func signedInPage(message string) pagekit.Page {
	return pagekit.Page{
		Title:   "Signed in to Giant Swarm",
		Heading: "Signed in",
		Summary: "Giant Swarm is now connected",
		Message: message,
	}
}

// renderFailed writes a sign-in error page. summary names the cause, message
// says what to do next, and errorCode is shown when it is a known OAuth
// error code.
func renderFailed(w http.ResponseWriter, status int, summary, message, errorCode string) {
	p := pagekit.Page{
		Heading: "Sign-in failed",
		Summary: summary,
		Message: message,
	}
	if knownErrorCodes[errorCode] {
		p.Detail = errorCode
	}
	pagekit.Render(w, status, p)
}
