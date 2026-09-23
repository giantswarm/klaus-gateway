package channels

import (
	"strings"
)

// FailureClass names what a failed turn broke on: the note a channel posts
// and the `failure_class` of the turn_complete record and of
// `klaus_gateway_turn_total`. A burst of one class is an outage of that part
// of the platform, which a single model error never looks like.
type FailureClass string

const (
	// FailureNone is the class of a turn that did not fail.
	FailureNone FailureClass = ""
	// FailureTools: the agent could not set up or reach its tools (the MCP
	// tool set's initialize or tool listing, an MCP server asking for
	// authorization).
	FailureTools FailureClass = "tools"
	// FailurePlatform: the connection between the gateway, the controller and
	// the agent's runtime broke before the agent could work.
	FailurePlatform FailureClass = "platform"
	// FailureModel: the model or its provider returned an error.
	FailureModel FailureClass = "model"
	// FailurePolicy: a gateway policy refused the request (authorization, a
	// prompt guard, a rate limit).
	FailurePolicy FailureClass = "policy"
	// FailureUnknown: nothing above matched.
	FailureUnknown FailureClass = "unknown"
)

// The markers ClassifyFailure looks for, lower case. The runtime reports a
// failure as the text of its error chain, so the wording of the layer that
// wraps it — the Go ADK's tool set and model call, the providers' SDKs,
// agentgateway's refusals, Go's net package — is the only signal there is.
var (
	toolSetMarkers = []string{
		"tool set", // the ADK's "failed to extract tools from the tool set"
		"mcp session",
		"mcp tools",
		"authorization required",
	}
	policyMarkers = []string{
		"authorization failed", // agentgateway's authorization policy
		"rejected due to inappropriate content",
		"rate limit exceeded",
		"code = permissiondenied",
	}
	modelMarkers = []string{
		"failed to call model",
		"anthropic api error",
		"openai chat completion request failed",
		"openai responses request failed",
		"sap ai core",
		"invalid_request_error",
		"overloaded_error",
		"rate_limit_error",
		"resource_exhausted",
	}
	transportMarkers = []string{
		"connection reset",
		"connection refused",
		"broken pipe",
		"no such host",
		"code = unavailable",
	}
)

// ClassifyFailure returns the class of a turn's failure, FailureNone for a
// nil error. The order matters: the tool set's failure quotes its transport
// error and may quote a refusal, a model call's failure may quote its
// transport error, and a refusal on the way to the model is a policy's, not
// the model's.
func ClassifyFailure(err error) FailureClass {
	if err == nil {
		return FailureNone
	}
	text := strings.ToLower(err.Error())
	switch {
	case containsAny(text, toolSetMarkers):
		return FailureTools
	case containsAny(text, policyMarkers):
		return FailurePolicy
	case containsAny(text, modelMarkers):
		return FailureModel
	case containsAny(text, transportMarkers):
		return FailurePlatform
	}
	return FailureUnknown
}

// Retryable reports whether a second attempt of a turn that failed with
// class may get past the failure: a connection that broke may hold the next
// time, a model's or a policy's answer comes back the same.
func (c FailureClass) Retryable() bool {
	return c == FailureTools || c == FailurePlatform
}

func containsAny(s string, markers []string) bool {
	for _, m := range markers {
		if strings.Contains(s, m) {
			return true
		}
	}
	return false
}
