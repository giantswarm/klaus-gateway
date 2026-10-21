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
	pagekit.Render(w, status, pagekit.Page{
		Title:   "Sign-in to Giant Swarm failed",
		Heading: "Sign-in failed",
		Summary: summary,
		Message: message,
		Detail:  pagekit.OAuthErrorCode(errorCode),
	})
}
