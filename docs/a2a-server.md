# A2A server

The gateway's A2A server lets an outside A2A client, such as an application's backend
acting for its own users, talk to the platform's agents over
[A2A v1.0](https://a2a-protocol.org/v1.0.1/specification/) JSON-RPC. The A2A client never
reaches kagent: the gateway authenticates it, exchanges its token for a platform token and
runs the turn through the same kagent client as the Slack channel.

It is off by default. It needs `a2a.enabled` and `obo.musterUrl` (a Session is started
through agent-manager, called through muster). `obo.enabled` is not needed.

## Endpoints

Both are served on the gateway's public listener (`server.port`), next to the Slack
endpoints. The installation's ingress or agentgateway route must send them to the gateway.

| Path | What |
|---|---|
| `/.well-known/agent-card.json` | Public Agent Card: the `/a2a` interface and one security scheme per trusted issuer, any one of which is enough. Needs no token. |
| `/a2a` | A2A JSON-RPC. Every request needs `Authorization: Bearer <token>`. The body is read only after the token is verified: a request without a valid token gets 401 with its body unread. A body larger than `a2aServer.maxBodyBytes` (2 MiB by default) is then refused with 413 before it is read as JSON or the token is exchanged. The authenticated extended card lists one interface per agent the caller may use. |

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

See the [metadata contract](a2a-metadata-contract.md) for top-level key validation, transport-owned fields and collision rules. Forwarding data does not implement an extension's behavior or replace capability negotiation for extensions that require it.

## How a request is authenticated

The gateway trusts the issuers listed in `a2aServer.issuers`: the installation's own Dex, for
applications that get a Dex token, and any outside identity provider whose tokens Dex
can exchange through a connector. The exchange itself always runs at the installation's
Dex (`a2aServer.exchange.tokenUrl`).

1. The application gets a token from one of the trusted issuers for that issuer's
   `audience`. From Dex, for example with `client_credentials` and the scope
   `audience:server:client_id:<audience>`.
2. The gateway reads the token's `iss` without trusting it and picks the issuer entry
   with exactly that `issuer`. A token of any other issuer is refused (401
   `invalid_token`).
3. With that entry alone, the gateway checks the token's signature (against the entry's
   `jwksUrl`, with a pinned asymmetric algorithm), issuer, `aud` (must contain the entry's
   `audience` exactly), expiry, not-before and issued-at, with mcp-oauth's OIDC token validation.
   For ID tokens, the requesting client is `azp`, or the sole `aud` entry when `azp` is absent.
   For access tokens, it is `client_id` (RFC 9068), with `azp` supported for providers that
   supply that claim alone. Conflicting `client_id` and `azp` values are refused for access tokens,
   and the resource audience is never used as their client identity. Access-token support uses
   these JWT conventions; it does not enforce the complete RFC 9068 profile's `typ`, `jti`
   and other required claims. A token requested by `a2aServer.exchange.clientId` is a platform
   token and is refused, whichever issuer signed it. Only tokens of the entry's `allowedClients`
   are accepted; every entry must list at least one. It does not check groups.
4. The gateway exchanges the token at `a2aServer.exchange.tokenUrl` (RFC 8693), authenticated
   as `a2aServer.exchange.clientId`, through the entry's `connectorId`, for the audiences in
   `a2aServer.exchange.audiences`. The subject token type is the entry's `subjectTokenType`
   (`urn:ietf:params:oauth:token-type:id_token` by default, or
   `urn:ietf:params:oauth:token-type:access_token`); the requested token type is always
   `urn:ietf:params:oauth:token-type:id_token`. The exchange endpoint must use HTTPS without
   userinfo or a fragment; redirects are refused, and each exchange has a ten-second deadline.
   This Dex exchange profile requires an issued ID token with `token_type=Bearer`
   (case-insensitive). Other usage types, including `N_A` and `DPoP`, are unsupported.
5. The exchanged token must have at least `a2aServer.minTokenLifetime` (25 minutes by
   default) left: the agent runtime keeps it for the whole turn and cannot renew it. Its
   lifetime is the earlier of the exchange response's `expires_in` (RFC 8693 section 2.2.1)
   and the token's `exp`. A shorter or unknown lifetime is refused with 502 and logged as
   an error, since it means the identity provider issues tokens shorter than a turn.
6. The turn runs with the exchanged token only. The application's token is never sent
   on.

The audience check in step 3 is the gateway's own: Dex does not check the subject
token's audience in an exchange (dexidp/dex#4397). The exchanged token's expiry is
Dex's, not the subject token's.

A later request with the same application token reuses the exchanged token, while the
application token is still valid and the exchanged token still has
`a2aServer.minTokenLifetime` left; otherwise the gateway exchanges again. A client that
polls `GetTask` therefore causes one exchange per token, not one per poll. The gateway
keeps at most 10,000 exchanged tokens in memory, keyed by the SHA-256 of the application
token, and never longer than the application token is valid.

The exchange is where the identity provider decides who may use the channel, so its
configuration below is part of the channel's security, not an option.

## Sign-ins and tool approvals

muster can ask the person behind a token to act before a tool runs: sign in to a
connector, or approve the call (tool approval, giantswarm/muster#1912). The person is
whoever the exchanged token names, and only that person can complete the step, on
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

The public card has one security scheme per issuer, named by the entry's `name`, and one
security requirement per scheme: a token of any one issuer is enough.

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
required for the issuer entry of the installation's own Dex. An outside issuer's entry
needs items 2, 4, 5 and 7, applied to its own connector, and the connector described
under [Outside issuers](#outside-issuers).

1. **A separate exchange client.** `a2aServer.exchange.clientId` must not be the Dex entry's
   `audience`. Register one client as the audience applications request (`klaus-gateway-app` below)
   and another that the gateway authenticates as for the exchange
   (`klaus-gateway-exchange`). Otherwise every exchanged token also carries
   `aud: klaus-gateway-app`. The gateway refuses it by its `azp`, but the self connector
   would still accept it as a subject token from any client allowed to use it.
2. **Each platform audience trusts the exchange client.** Every client in
   `a2aServer.exchange.audiences` lists the exchange client in `trustedPeers`, not the
   audience client. The audience client lists the application clients in
   `trustedPeers`.
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
   or every request is refused (step 5). Keep it no longer than that needs: an exchanged
   token can itself be exchanged again while it is valid (dexidp/dex#3546).

When dexidp/dex#5165 is released, also set `validateTokenExchangeAudience: true` with
`clientID: <audience>` (the Dex entry's) on the self connector. Together with item 1 it refuses any
subject token not issued for the application audience, exchanged tokens included.

Each entry's `allowedClients` (the application clients, `example-app` below) refuses
tokens other clients request for the application audience; items 1 and 3 still keep
the self connector from exchanging them.

Example, with one application client `example-app` (secrets omitted):

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
    name: klaus-gateway token exchange
    allowedConnectors: [dex-self]
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

An issuer that sends access tokens sets
`subjectTokenType: urn:ietf:params:oauth:token-type:access_token` on its entry.
Dex's `oidc` connector verifies only ID tokens itself: it exchanges an access token by
calling the issuer's userinfo endpoint with it (dex `connector/oidc/oidc.go:612-625`),
so the connector needs `getUserInfo: true` and the issuer a userinfo endpoint that
answers for those access tokens. The gateway still checks the access token's
signature, issuer, audience, expiry and client itself (step 3), so it must be a JWT.

The connector trusts that issuer only, so a Dex token cannot be exchanged through it,
and the issuer's tokens cannot be exchanged through `dex-self`. The issuer must publish
its key set where the gateway reaches it (`jwksUrl`), and Dex must reach its discovery
document. The claim Dex maps to the exchanged token's `email` must be a stable
identifier the issuer sets, not a display name.

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
  issuers:
    # The installation's Dex, for A2A clients with a Dex client.
    - name: dex
      issuer: https://dex.example.com
      jwksUrl: https://dex.example.com/keys
      tokenUrl: https://dex.example.com/token
      audience: klaus-gateway-app
      allowedClients: [example-app]
      connectorId: dex-self
    # An application's own token service, exchanged through its connector.
    - name: example-idp
      issuer: https://idp.example.org
      jwksUrl: https://idp.example.org/keys
      audience: klaus-gateway-outside
      allowedClients: [example-backend]
      connectorId: example-idp
  exchange:
    tokenUrl: https://dex.example.com/token
    clientId: klaus-gateway-exchange
    # Secret in the release namespace, key client-secret.
    existingSecret: klaus-gateway-exchange-client
    # kagent checks its own audience; muster and the Kubernetes API server
    # check dex-k8s-authenticator.
    audiences: [kagent, dex-k8s-authenticator]
  # Largest /a2a request body, 2 MiB by default.
  maxBodyBytes: 2097152
  # Least lifetime an exchanged token must have left; Dex's expiry.idTokens
  # must be longer.
  minTokenLifetime: 25m
  # Secret with key ca.crt, when the exchange's token endpoint is not publicly
  # trusted. An issuer entry names it as caFile: /etc/klaus-gateway/a2a-server-ca/ca.crt.
  caSecret: ""
```

The gateway fetches each `jwksUrl` with mcp-oauth's SSRF-safe client: the URL must use
`https`, and a host that resolves to a private, loopback or link-local address is refused.
An identity provider reached inside the cluster lists its hostname in the entry's
`allowPrivateIpJwksHosts` (for example `dex.dex.svc.cluster.local`), which admits private
addresses (RFC 1918, RFC 4193) for that host only; loopback and link-local stay refused.

The chart renders `a2aServer.issuers` into the ConfigMap `<release>-a2a-server-issuers`, read with
`--a2a-server-issuers-file`; a change to it rolls the pods. The gateway refuses to start on an
unknown field in an entry, a missing `name`, `issuer`, `jwksUrl`, `audience` or `allowedClients`, or a
`name` or `issuer` used twice. The render fails when `a2aServer.enabled` is set without
`a2a.enabled`, `publicUrl`, an issuer, `exchange.tokenUrl`, `exchange.clientId`,
`exchange.existingSecret`, `exchange.audiences` or `obo.musterUrl`. The chart never
renders the exchange client's secret.

## Flags

| Flag | Meaning |
|---|---|
| `--a2a-server-issuers-file` | YAML list of issuer entries, the fields of `a2aServer.issuers`. |
| `--a2a-server-exchange-token-url` | Token endpoint the exchange runs at. |
| `--a2a-server-exchange-client-id`, `--a2a-server-exchange-client-secret-file`, `--a2a-server-exchange-audiences` | The exchange client and the audiences it asks for. |
| `--a2a-server-ca-file` | CA bundle for the exchange's token endpoint. |
| `--a2a-server-max-body-bytes` | Largest `/a2a` request body; 2 MiB by default. |
| `--a2a-server-min-token-lifetime` | Least lifetime an exchanged token must have left; 25m by default, at least 1m. |
