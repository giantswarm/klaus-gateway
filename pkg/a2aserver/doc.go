// Package a2aserver is the gateway's A2A v1.0 server for outside A2A
// clients, such as an application's backend acting for its own users. An A2A
// client never reaches the agent runtime behind the gateway.
//
// The A2A client authenticates at agentgateway with an OAuth 2.0 bearer token
// (RFC 6750) that one of the trusted issuers issued for that issuer's
// audience. agentgateway verifies it and exchanges it at the installation's
// identity provider for a platform token (RFC 8693); the A2A client's token
// never reaches the gateway. The gateway verifies the platform token
// (PlatformVerifier) and acts with it unchanged.
//
// An agent is addressed as <organization>/<agent>: by its own endpoint and
// Agent Card under /a2a (NewAgentPaths), or by the A2A tenant on /a2a. A message without a
// context id starts a platform Session through agent-manager; the Session id
// is the context id of every later message.
//
// Request-level application metadata is forwarded directly to MCP tool calls.
// It requires no custom extension, wrapper or activation header. Extensions
// the caller requests (A2A-Extensions) are requested on the agent's call too.

package a2aserver
