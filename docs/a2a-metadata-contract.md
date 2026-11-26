# Request-scoped MCP metadata

The gateway forwards the optional A2A bridge proposed in
[kagent-dev/kagent#3791](https://github.com/kagent-dev/kagent/pull/3791).
Its identifier is `https://kagent.dev/extensions/mcp-request-metadata/v1`.
This is a project-defined A2A extension, not a standard MCP extension.
The runtime copies the envelope's entries directly into `tools/call._meta`.
It adds no MCP carrier object, capability or application-specific interpretation.

For example, an application can supply its own metadata object:

```json
{
  "metadata": {
    "https://kagent.dev/extensions/mcp-request-metadata/v1": {
      "io.giantswarm/tool-call-metadata": {"session": "123", "format": "csv"},
      "com.example/request-id": "request-456"
    }
  }
}
```

The client activates the bridge using `A2A-Extensions` with the bridge URI.
The MCP tool receives the two entries directly in `_meta`. The Giant Swarm key
is an application convention in this example; the bridge neither defines its
schema nor advertises support for an extension with that identifier.
Applications and servers own the meaning and validation of their values.

Each tool called during that A2A request can receive the full object, including
servers without application extension declarations. There are no per-server
policies or per-field filters. Do not supply confidential values on the
assumption that only one server will receive them.

Only `tools/call` receives the entries. Discovery, initialization and list
requests receive none. Missing, null and empty envelopes contribute no values.
An inactive bridge contributes none. Follow-up and approval requests supply
their own values; a request without them inherits nothing.

The gateway and runtime validate top-level keys against MCP's `_meta` naming syntax.
Names inside an application object are unconstrained by the bridge.
They refuse transport-owned fields before dispatch: `progressToken`, `traceparent`,
`tracestate`, `baggage` and the MCP core fields
`io.modelcontextprotocol/protocolVersion`, `io.modelcontextprotocol/clientInfo`,
`io.modelcontextprotocol/clientCapabilities`, `io.modelcontextprotocol/logLevel`,
`io.modelcontextprotocol/serverInfo` and `io.modelcontextprotocol/subscriptionId`.
Existing outgoing metadata wins on other collisions without merging
nested objects.

Forwarding data does not implement an MCP extension, negotiate its capabilities,
or make unsupported extension behavior available. Extensions requiring extra
client behavior must be implemented by that client separately. The bridge
leaves authentication and authorization on their existing transports. Values
are untrusted application data, never credentials.

The [A2A extension rules](https://a2a-protocol.org/v1.0.1/topics/extensions/)
define declaration and activation. MCP defines the
[`_meta` field](https://modelcontextprotocol.io/specification/2026-07-28/basic#_meta)
and [extension negotiation](https://modelcontextprotocol.io/specification/2026-07-28/basic/versioning#extension-negotiation)
separately. The implementation is provisional until the upstream proposal is
accepted; the identifier does not imply that a specification is hosted at its URI.
