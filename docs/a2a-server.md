# A2A server

The gateway's A2A server lets an outside A2A client, such as an application's backend
acting for its own users, talk to the platform's agents over
[A2A v1.0](https://a2a-protocol.org/v1.0.1/specification/) JSON-RPC. The A2A client never
reaches kagent: it reaches the gateway through agentgateway, which checks its token and
exchanges it for a platform token. The gateway checks the platform token and runs the
turn with it through the same kagent client as the Slack channel.

It is off by default. It needs `a2a.enabled` and `obo.musterUrl` (a Session is started
through agent-manager, called through muster). `obo.enabled` is not needed.

## Endpoints

Both are served on the gateway's public listener (`server.port`), next to the Slack
endpoints. agentgateway's routes, rendered by agent-platform, send them to the gateway.

| Path | What |
|---|---|
| `/.well-known/agent-card.json` | Public Agent Card: the `/a2a` interface and one security scheme per trusted issuer, any one of which is enough. Needs no token. |
| `/a2a` | A2A JSON-RPC. Every request needs `Authorization: Bearer <platform token>`. The body is read only after the token is verified: a request without a valid token gets 401 with its body unread. A body larger than `a2aServer.maxBodyBytes` (2 MiB by default) is then refused with 413 before it is read as JSON. The authenticated extended card lists one interface per agent the caller may use. |

Every call must send `A2A-Version: 1.0`: a call without it is an A2A 0.3 call (A2A 1.0
section 3.6.2) and is refused with `-32009` (version not supported).

An agent is addressed by the A2A `tenant` as `<organization>/<agent>`. Only agents of an
organization are offered; an unknown or unoffered tenant is refused with the same error.
A message without a context id starts a platform Session, and the Session id is the
context id of every later message.

Non-streaming sends use the controller's native A2A `SendMessage`: it owns background
task execution and honors `configuration.returnImmediately`. Sends and streaming task
snapshots honor `configuration.historyLength`; `GetTask` honors `historyLength`.
Zero omits history, a positive limit returns at most that many recent messages, and
an omitted limit leaves history unrestricted. Negative limits are refused. Recognized
downstream A2A errors retain their protocol codes with safe descriptions.

## Request metadata

Request-level `params.metadata` is forwarded unchanged to the agent runtime, which adds its application entries directly to MCP `tools/call` request `_meta`:

```json
{"metadata":{"com.example/session":{"sid":"<A2A context id>"},"format":"csv"}}
```

No custom A2A or MCP extension, URI, activation header, capability declaration or wrapper is required. Message and part metadata retain their existing meanings. Every called MCP server can receive the full application metadata; there is no application-key or destination filtering. Values are application data, never credentials.

Each follow-up or approval request supplies its own values. A request without metadata inherits nothing. Values are added after the model chooses a tool, outside prompts and tool arguments; a server can still expose them to the model by echoing them in results.

See the [metadata contract](a2a-metadata-contract.md) for top-level key validation, transport-owned fields and collision rules.

## Extensions

The extensions an A2A client requests with the `A2A-Extensions` header are requested on the
agent's call too, next to kagent's HITL extension, which the gateway always requests. The
agent runtime decides which extensions it activates. Request metadata stays bound for
MCP `_meta` and always gets its key validation, including when an extension with the
same name is requested. An extension's URI-keyed data can travel in message or part
metadata; the gateway does not forward that metadata to MCP.

This forwards extension requests only. The pinned A2A SDK does not expose successful
gRPC response headers to its client interceptors or write activation acknowledgements
from its JSON-RPC server. The gateway therefore returns no `A2A-Extensions` response
header and does not offer complete extension negotiation
([a2aproject/a2a-go#643](https://github.com/a2aproject/a2a-go/issues/643)). The public
Agent Card declares no extensions.

## How a request is authenticated

Two components check tokens. agentgateway admits the A2A client's token and exchanges it;
the gateway admits only the platform token that exchange issued.

**At agentgateway** (agent-platform renders one `AgentgatewayPolicy` on the `/a2a` route):

1. The application gets a token from one of the trusted issuers for that issuer's
   `audience`: the installation's own Dex (for example with `client_credentials` and the
   scope `audience:server:client_id:<audience>`), or an outside identity provider that
   Dex can exchange tokens of through a connector.
2. JWT authentication, mode `Strict`, with one provider per trusted issuer. A provider
   checks the signature against its issuer's key set, the issuer, the expiry and an
   exact `aud`. A token of any other issuer is refused with 401.
3. An authorization rule checks the requesting client per issuer: `azp`, or the sole
   `aud` entry when `azp` is absent, must be one of that issuer's allowed clients.
4. Backend auth `oauthTokenExchange` exchanges the token at the installation's Dex
   (RFC 8693), authenticated as the exchange client (`klaus-gateway-exchange` below). The
   connector is picked by the token's `iss` (`additionalParams.connector_id`, a CEL
   expression), the audiences are Dex scopes `audience:server:client_id:<aud>`, and the
   requested token type is `urn:ietf:params:oauth:token-type:id_token`. The subject token
   type is one value for every issuer. The exchange's token cache is off
   (`cache.inMemory.maxEntries: 0`): agentgateway reuses a cached token until shortly
   before it expires, which does not leave a turn its minimum lifetime.
5. The exchanged token replaces the application's token. The application's token is never
   sent to the gateway.

**At the gateway** (`a2aServer.token`):

6. With mcp-oauth's OIDC token validation, the gateway checks the platform token's
   signature (against `a2aServer.token.jwksUrl`, with a pinned asymmetric algorithm),
   issuer (`a2aServer.token.issuer`, exactly), `aud` (must contain
   `a2aServer.token.audience` exactly), expiry, not-before and issued-at. The requesting
   client is `azp`, or the sole `aud` entry when `azp` is absent, and must be one of
   `a2aServer.token.allowedClients`: the exchange client. Any other token, an
   application's token sent past agentgateway included, is refused with 401.
7. The token must have at least `a2aServer.minTokenLifetime` (25 minutes by default) left:
   the agent runtime keeps it for the whole turn and cannot renew it. A token with less
   left, or without `exp`, is refused with 502 and logged as an error. This is an
   admission floor, not a turn timeout or token renewal: a turn that outlasts the
   remaining lifetime can still fail.
8. The turn runs with the platform token, unchanged.

Dex does not check the subject token's audience in an exchange (dexidp/dex#4397), so
agentgateway's audience check in step 2 is what enforces it. Every request is exchanged on
its own: a client that polls `GetTask` causes one exchange per poll.

The exchange is where the identity provider decides who may use the channel, so its
configuration below is part of the channel's security, not an option.

muster runs a call only as a token the Kubernetes API accepts: it must carry the
audience the API server checks (`kubernetes` on Giant Swarm installations). The exchange
must request that audience next to `a2aServer.token.audience`. Without it, muster answers
every call with its re-login error, and `SendMessage` and `SendStreamingMessage` answer
with the A2A error `UNAUTHENTICATED`. Its `google.rpc.ErrorInfo` detail carries muster's
words in `metadata.reason`. muster identifies this refusal with its MCP tool
result's `structuredContent.status: unauthenticated`; the gateway does not parse
the text. This requires muster 5.67.0-rc.18 or later. A new application token does
not help: the exchange's audiences must be fixed.

## Sign-ins and tool approvals

muster can ask the person behind a token to act before a tool runs: sign in to a
connector, or approve the call (tool approval, giantswarm/muster#1912). The person is
whoever the platform token names, and only that person can complete the step, on
muster's page.

- **Before the agent starts.** When muster asks for a step on agent-manager's
  `start_session`, `SendMessage` and `SendStreamingMessage` answer with the A2A error
  `UNAUTHORIZED`. Its `google.rpc.ErrorInfo` detail carries the step in `metadata`:
  `url` (the page to open) and `message` (muster's words, to show as written). After the
  step, the client sends the same message again, with the same `messageId`: agent-manager
  starts one Session per message id, and muster's decision applies to that retry.
- **During a turn.** The agent runtime relays muster's URL elicitation as kagent's HITL
  extension: the task stops at `TASK_STATE_INPUT_REQUIRED`, and its status message carries
  a `url_elicitation_request` (`id`, `server`, `message`, `url`, `elicitation_id`) under
  `https://kagent.dev/extensions/hitl/v1` in its metadata. The text part also carries the
  URL. The client answers the task with a `url_elicitation_response` (`accept` or
  `decline`), as described in [kagent-a2a.md](kagent-a2a.md). A2A 1.0 names
  `TASK_STATE_AUTH_REQUIRED` for this (section 7.6); the runtime uses
  `TASK_STATE_INPUT_REQUIRED` because a2a-go ends a streamed execution only on that state
  (kagent-dev/kagent#2326).

Who can complete a step:

- **A person using an application** (a chat bot that sends its user's token from an
  outside issuer): that person, who must be able to sign in to muster with the same
  subject the exchange gives them. With Dex, that means signing in through the same
  connector, so the connector also needs the `authorization_code` grant, next to the token
  exchange grant of item 4.
- **An application acting as itself** (a `client_credentials` token): nobody. No person
  can sign in as an application, so a tool that asks for approval does not run for it.
  Approvals for application identities are not supported.

## Agent Card

The public card has one security scheme per entry of `a2aServer.issuers`, named by the
entry's `name`, and one security requirement per scheme: a token of any one issuer is
enough. The entries name the issuers only; list the same issuers in agentgateway's JWT
providers, which check their tokens.

| Entry sets | Scheme |
|---|---|
| `tokenUrl` | OAuth 2.0, `clientCredentials` flow at `tokenUrl`. `oauth2MetadataUrl` only when the entry sets `metadataUrl`, an RFC 8414 document. |
| no `tokenUrl` | OpenID Connect, `openIdConnectUrl` = the entry's `openIdConnectUrl`, by default `<issuer>/.well-known/openid-configuration`. |

`oauth2MetadataUrl` names RFC 8414 metadata only, and Dex serves only OpenID Connect
discovery, so a Dex entry never sets `metadataUrl`. `metadataUrl` needs `tokenUrl`, and
`openIdConnectUrl` excludes it. The card never names the exchange's token endpoint for
an outside issuer.

## Identity provider configuration (Dex)

Dex's token exchange through an `oidc` connector checks the subject token's signature,
issuer and expiry, but not its audience (dexidp/dex#4397, fix proposed in
dexidp/dex#5165). Dex also adds the requesting client to the `aud` of every
cross-client token (dexidp/dex#1087). With Dex as it is today, all of the following are
required for the installation's own Dex as a trusted issuer. An outside issuer needs
items 2, 4, 5 and 7, applied to its own connector, and the connector described under
[Outside issuers](#outside-issuers).

1. **A separate exchange client.** The exchange client agentgateway authenticates as
   (`klaus-gateway-exchange` below) must not be the audience applications request
   (`klaus-gateway-app` below). Otherwise every exchanged token also carries
   `aud: klaus-gateway-app`, and the self connector would accept it as a subject token
   from any client allowed to use it.
2. **Each platform audience trusts the exchange client.** Every audience agentgateway's
   exchange requests (`a2aServer.token.audience` among them) lists the exchange client in
   `trustedPeers`, not the application audience client. The application audience client
   lists the application clients in `trustedPeers`.
3. **`allowedGroups` on the self connector.** The `oidc` connector that points at Dex
   itself accepts only tokens whose `groups` contain a group that application clients
   alone get (through `clientCredentialsClaims`). No static user, other connector or
   other client may emit that group, so give it a name that cannot collide with an
   upstream identity provider's group.
4. **Connector `grantTypes`.** Set the self connector's `grantTypes` to the token
   exchange grant only, so it never appears as a login option.
5. **`allowedConnectors` on every other client.** A client without `allowedConnectors`
   may use every connector, including the self connector. Give every other confidential
   client (the kagent and Kubernetes clients included) an `allowedConnectors`
   list without it, so only the exchange client can exchange application tokens.
6. **`client_credentials` in `oauth2.grantTypes`.** Dex v2.46 does not enable it by
   default. It is installation-wide: every confidential client can then use it.
7. **Token lifetime.** `expiry.idTokens` must be longer than `a2aServer.minTokenLifetime`,
   or every request is refused (step 7). Keep it no longer than that needs: an exchanged
   token can itself be exchanged again while it is valid (dexidp/dex#3546).

When dexidp/dex#5165 is released, also set `validateTokenExchangeAudience: true` with
`clientID: <audience>` (the application audience) on the self connector. Together with
item 1 it refuses any subject token not issued for the application audience, exchanged
tokens included.

agentgateway's allowed clients per issuer (the application clients, `example-app` below)
refuse tokens other clients request for the application audience; items 1 and 3 still
keep the self connector from exchanging them.

Example, with one application client `example-app` and the platform token's audience
`klaus-gateway` (secrets omitted):

```yaml
oauth2:
  grantTypes:
    - authorization_code
    - refresh_token
    - urn:ietf:params:oauth:grant-type:token-exchange
    - client_credentials

expiry:
  idTokens: 30m

connectors:
  - id: dex-self
    type: oidc
    name: Applications
    grantTypes:
      - urn:ietf:params:oauth:grant-type:token-exchange
    config:
      issuer: https://dex.example.com
      clientID: klaus-gateway-app
      redirectURI: https://dex.example.com/callback
      scopes: [openid, profile, groups]
      insecureEnableGroups: true
      allowedGroups: [platform-apps-example]
      claimMapping:
        email: preferred_username

staticClients:
  - id: example-app
    name: Example application
    allowedConnectors: []
    clientCredentialsClaims:
      groups: [platform-apps-example]
  - id: klaus-gateway-app
    name: klaus-gateway applications
    trustedPeers: [example-app]
    allowedConnectors: [dex-self]
  - id: klaus-gateway-exchange
    name: klaus-gateway token exchange (agentgateway)
    allowedConnectors: [dex-self]
  - id: klaus-gateway
    trustedPeers: [klaus-gateway-exchange]
    allowedConnectors: [github]
  - id: kagent
    trustedPeers: [klaus-gateway-exchange]
    allowedConnectors: [github]
  - id: dex-k8s-authenticator
    trustedPeers: [klaus-gateway-exchange]
    allowedConnectors: [github]
```

`claimMapping.email: preferred_username` gives the exchanged token an `email`: a
`client_credentials` token has none, and the Kubernetes API server and agentgateway's
kagent route key on it. Replace `github` with the connectors people sign in with.

## Outside issuers

An outside issuer (an application's own token service) is trusted through its own Dex
connector, exchange only:

```yaml
connectors:
  - id: example-idp
    type: oidc
    name: Example token service
    grantTypes:
      - urn:ietf:params:oauth:grant-type:token-exchange
    config:
      issuer: https://idp.example.org
      clientID: klaus-gateway-outside
      redirectURI: https://dex.example.com/callback
      scopes: [openid, profile, groups]
      insecureEnableGroups: true
      allowedGroups: [platform-apps-example]

staticClients:
  - id: klaus-gateway-exchange
    allowedConnectors: [dex-self, example-idp]
```

agentgateway names one subject token type for every issuer. Dex's `oidc` connector
verifies only ID tokens itself: it exchanges an access token by calling the issuer's
userinfo endpoint with it (dex `connector/oidc/oidc.go:612-625`), so an issuer that sends
access tokens needs the connector's `getUserInfo: true`, a userinfo endpoint that answers
for those access tokens, and every other issuer sending access tokens too. agentgateway
still checks the token's signature, issuer, audience, expiry and client itself (steps 2
and 3), so it must be a JWT.

The connector trusts that issuer only, so a Dex token cannot be exchanged through it,
and the issuer's tokens cannot be exchanged through `dex-self`. The issuer must publish
its key set where agentgateway reaches it, and Dex must reach its discovery document. The
claim Dex maps to the exchanged token's `email` must be a stable identifier the issuer
sets, not a display name.

## Chart values

```yaml
a2a:
  enabled: true
  url: grpc://agentgateway.agent-platform.svc.cluster.local:8080

obo:
  musterUrl: http://muster.agent-platform.svc.cluster.local:8090

a2aServer:
  enabled: true
  publicUrl: https://klaus-gateway.example.com
  # The issuers the Agent Card declares; agentgateway checks their tokens.
  issuers:
    # The installation's Dex, for A2A clients with a Dex client.
    - name: dex
      issuer: https://dex.example.com
      tokenUrl: https://dex.example.com/token
      audience: klaus-gateway-app
    # An application's own token service.
    - name: example-idp
      issuer: https://idp.example.org
      audience: klaus-gateway-outside
  # The platform token agentgateway's exchange issues.
  token:
    issuer: https://dex.example.com
    jwksUrl: https://dex.example.com/keys
    audience: klaus-gateway
    allowedClients: [klaus-gateway-exchange]
  # Largest /a2a request body, 2 MiB by default.
  maxBodyBytes: 2097152
  # Least lifetime the platform token must have left; Dex's expiry.idTokens
  # must be longer.
  minTokenLifetime: 25m
  # Secret with the CA bundle, when token.jwksUrl is not publicly trusted.
  caSecret: ""
  caSecretKey: ca.crt
```

The gateway fetches `a2aServer.token.jwksUrl` with mcp-oauth's SSRF-safe client: the URL
must use `https`, and a host that resolves to a private, loopback or link-local address
is refused. An identity provider reached inside the cluster lists its hostname in
`a2aServer.token.allowPrivateIpJwksHosts` (for example `dex.dex.svc.cluster.local`), which
admits private addresses (RFC 1918, RFC 4193) for that host only; loopback and link-local
stay refused.

The chart renders `a2aServer.issuers` into the ConfigMap `<release>-a2a-server-issuers`,
read with `--a2a-server-issuers-file`; a change to it rolls the pods. The values schema and
the gateway refuse an unknown field in an entry, a missing `name`, `issuer` or `audience`,
or a `name` or `issuer` used twice. The render fails when `a2aServer.enabled` is set
without `a2a.enabled`, `publicUrl`, an issuer, `token.issuer`, `token.jwksUrl`,
`token.audience`, `token.allowedClients` or `obo.musterUrl`.

## Flags

| Flag | Meaning |
|---|---|
| `--a2a-server-issuers-file` | YAML list of the issuers the Agent Card declares, the fields of `a2aServer.issuers`. |
| `--a2a-server-token-issuer`, `--a2a-server-token-jwks-url`, `--a2a-server-token-audience` | The platform token's issuer, its HTTPS key set and the audience the token must carry. |
| `--a2a-server-token-allowed-clients` | Comma-separated clients whose platform tokens are accepted: agentgateway's exchange client. |
| `--a2a-server-token-allow-private-ip-jwks-hosts` | Comma-separated hostnames whose key set URL may resolve to a private address. |
| `--a2a-server-ca-file` | CA bundle for the platform token's key set. |
| `--a2a-server-max-body-bytes` | Largest `/a2a` request body; 2 MiB by default. |
| `--a2a-server-min-token-lifetime` | Least lifetime the platform token must have left; 25m by default, at least 1m. |
