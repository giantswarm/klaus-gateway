// Package a2aserver is the gateway's A2A v1.0 server for outside A2A
// clients, such as an application's backend acting for its own users. An A2A
// client never reaches the agent runtime behind the gateway.
//
// The A2A client authenticates with an OAuth 2.0 bearer token (RFC 6750)
// that one of the trusted issuers issued for that issuer's audience. The
// gateway picks the issuer by the token's iss, verifies the token with that
// issuer alone, exchanges it at the installation's identity provider for a
// token meant for the platform (RFC 8693) and acts with that token only: the
// A2A client's token is never sent on.
//
// The A2A tenant names the agent as <organization>/<agent>. A message without a
// context id starts a platform Session through agent-manager; the Session id
// is the context id of every later message.
//
// The only A2A extension passed on to the agent runtime is kagent's MCP
// tool-call metadata extension (ToolCallMetadataExtensionURI): its request
// metadata entry, keyed by metadata key, and its activation travel with the
// turn unchanged, for the runtime to send to MCP servers that declare support.
// The payload is an opaque JSON object; every other request metadata entry is
// dropped.
package a2aserver
