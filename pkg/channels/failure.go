package channels

import (
	"errors"
	"strings"

	pkga2a "github.com/giantswarm/klaus-gateway/pkg/a2a"
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
	// prompt guard, a rate limit), or the sender may not use the agent.
	FailurePolicy FailureClass = "policy"
	// FailureConnectionRequired: the agent's model runs on a provider account
	// of the person's own (a ChatGPT subscription) that they have not
	// connected, so the egress refused to lend its credential.
	FailureConnectionRequired FailureClass = "connection_required"
	// FailureUnknown: nothing above matched.
	FailureUnknown FailureClass = "unknown"
)

// FailureReasoner is a failure that carries the runtime's own class of it,
// such as the reason kagent sets on a failed task's status message. A reason
// ClassifyFailure knows wins over the markers in the error's text.
type FailureReasoner interface {
	FailureReason() string
}

// The failure reasons a runtime reports that name a class.
var failureReasons = map[string]FailureClass{
	string(FailureConnectionRequired): FailureConnectionRequired,
}

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
		"llm error response", // the Go ADK's failed model response
		"invalid_request_error",
		"overloaded_error",
		"rate_limit_error",
		"resource_exhausted",
	}
	// The provider refused the platform's own credentials (Anthropic's error
	// types of a 401 and a 403): a model failure no retry gets past.
	credentialsMarkers = []string{
		"authentication_error",
		"permission_error",
	}
	// The egress gateway refusing to lend a person's provider credential:
	// "credential provider <authority> denied: <why>". Both words must be
	// there; the authority between them varies.
	connectionMarker, connectionDeniedMarker = "credential provider", "denied"
	transportMarkers                         = []string{
		"connection reset",
		"connection refused",
		"broken pipe",
		"no such host",
		"code = unavailable",
		"rst_stream", // the controller's HTTP/2 stream reset mid-turn (a restart)
	}
)

// ClassifyFailure returns the class of a turn's failure, FailureNone for a
// nil error. The runtime's own reason, when it names a class, decides;
// otherwise the order of the markers matters: a credential refusal quotes
// the model call it refused, the tool set's failure quotes its transport
// error and may quote a refusal, a model call's failure may quote its
// transport error, and a refusal on the way to the model is a policy's, not
// the model's.
func ClassifyFailure(err error) FailureClass {
	if err == nil {
		return FailureNone
	}
	if errors.Is(err, pkga2a.ErrAgentNotPermitted) {
		return FailurePolicy
	}
	var reasoner FailureReasoner
	if errors.As(err, &reasoner) {
		if class, ok := failureReasons[reasoner.FailureReason()]; ok {
			return class
		}
	}
	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, connectionMarker) && strings.Contains(text, connectionDeniedMarker):
		return FailureConnectionRequired
	case containsAny(text, toolSetMarkers):
		return FailureTools
	case containsAny(text, policyMarkers):
		return FailurePolicy
	case containsAny(text, modelMarkers), containsAny(text, credentialsMarkers):
		return FailureModel
	case containsAny(text, transportMarkers):
		return FailurePlatform
	}
	return FailureUnknown
}

// ModelCredentialsRefused reports whether err is a model failure in which the
// provider refused the platform's credentials, such as an invalid API key.
func ModelCredentialsRefused(err error) bool {
	return ClassifyFailure(err) == FailureModel && containsAny(strings.ToLower(err.Error()), credentialsMarkers)
}

// Retryable reports whether a second attempt of a turn that failed with
// class may get past the failure: a connection that broke may hold the next
// time, a model's or a policy's answer comes back the same, and a provider
// account stays unconnected until the person connects it.
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
