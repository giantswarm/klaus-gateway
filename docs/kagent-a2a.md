# The kagent integration: A2A v1 over gRPC, AgentTemplates, AgentInstances

klaus-gateway runs its agent conversations on a kagent API v2 controller. This page describes
the carrier under every channel: how the gateway finds agents, how a thread becomes a
conversation the controller holds, how a turn, a tool approval and a stop travel, and how the
gateway authenticates. The channel guides ([Slack](channels-slack.md), [Web](channels-web.md),
[CLI](channels-cli.md)) describe what a user sees; nothing there depends on the carrier.

## Wire

| Concern | Carrier |
|---|---|
| Turns | A2A v1 over gRPC (`lf.a2a.v1.A2AService`: `SendStreamingMessage`, `GetTask`, `CancelTask`) through the `a2a-go/v2` client on its gRPC transport |
| Agent roster | `kagent.api.v1alpha1.AgentTemplateService/ListAgentTemplates` |
| Conversations | `kagent.api.v1alpha1.AgentInstanceService` (`CreateAgentInstance`, `GetAgentInstance`, `DeleteAgentInstance`) |
| Model line (`/usage`) | `kagent.api.v1alpha1.ModelService/GetModelConfig` |

All of it goes to one gRPC target, `a2a.url`, reached through the platform's agentgateway:
`grpc://host:port` for plaintext h2c (the in-cluster agentgateway Service, e.g.
`grpc://agentgateway.agent-platform.svc.cluster.local:8080`) or `grpcs://host:port` for TLS
(the controller route's public hostname, e.g. `grpcs://agentgateway.<domain>:443`, with `a2a.caSecret` or
`a2a.caFile` for a private CA). The route in front of the controller must carry native gRPC
over HTTP/2 and preserve the `authorization` and `x-kagent-agent-instance-id` metadata.

The gateway makes no REST or JSON-RPC call to kagent and never fetches a `/.well-known`
AgentCard; the kagent service stubs it compiles against are generated from the controller's
protos (see `pkg/kagent/gen/README.md`).

## Identity

Every call is made **as the person behind the turn**: the Dex id_token the Slack account link
mints, or the bearer a web/cli caller sends, rides as the `authorization` metadata entry. The
gateway never presents its own ServiceAccount to the controller (`a2a.saToken` serves the
Klaus-instance paths only), so the controller route's JWT policy needs no second issuer, and
a turn without a person's token is refused instead of running as a machine identity.

Discovery is a person's call too. The roster is fetched as the caller and cached briefly
(`ListAgentTemplates` of `a2a.namespace`); reads that happen where no person's token is at hand
— branding a reply with the agent's display name and icon, recovering a thread's agent after a
restart — are served from that cache, which every authenticated call refreshes once it is
older than 30 seconds. Right after a start, before any turn has run, a roster listing without
a token reports the roster as unavailable until a turn has warmed the cache.

## Agents: the AgentTemplate roster

An agent is an `AgentTemplate` in `a2a.namespace` (default `kagent`). The roster a channel
offers is derived from `ListAgentTemplates`:

- technical name and namespace from the template's ref; `a2a.defaultAgent` is a bare name in
  that namespace (or `namespace/name`);
- display name from the `ui.giantswarm.io/display-name` annotation, icon from
  `ui.giantswarm.io/icon-url` (falling back to `a2a.fallbackIconUrlTemplate` with `{agent}`
  replaced by the technical name), description from the template;
- readiness from `status.harnesses[]`: a template is offered when a Harness admits it
  (`admitting_harnesses`) **and** that Harness reports a `Ready` condition for it. A template
  no Harness admits (it lacks the platform's admission label), or whose admitting Harness has
  not compiled a ready revision yet, is not offered; selecting it by name is refused with the
  reason in the gateway's log (the channel shows its unknown-agent notice with the roster).

The template's `modelConfig` reference backs the `/usage` model line through `GetModelConfig`
(`spec.provider/spec.model`).

## Conversations: one AgentInstance per thread

A conversation is an `AgentInstance` the controller creates and owns. The gateway binds each
channel thread to exactly one instance:

1. The thread's first turn calls `CreateAgentInstance` with the template, the admitting
   Harness the template's status reports, and `request_id` = the gateway's synthesized context
   id (a SHA-256 hex over `channel, channelID, "", threadID, agentRef`). The controller's create
   is idempotent per `(creator, request_id)`, so a retried first turn — the binding was not
   written, the process died in between — gets the same instance back instead of a second one.
   The call returns once the instance is READY.
2. The instance id is persisted in the routing store as the thread's entry
   (`store.Entry.AgentInstanceID`, the `agentInstanceID` field of a `ChannelRoute` CR; key
   `channel|channelID||threadID|agentRef`, user slot empty because the thread is shared by its
   participants). It survives a gateway restart on the bolt, ConfigMap and CRD stores and never
   expires on its own: the instance *is* the conversation, and the controller keeps it until
   it is deleted.
3. Every later turn of the thread routes to that instance: the id rides as the
   `x-kagent-agent-instance-id` metadata entry on each A2A call, exactly once. The message's
   `contextId` stays empty — the controller owns the conversation's context id and rejects any
   other value — and the controller assigns the task id of a fresh turn.

The controller runs one task per instance at a time. A turn it refuses because a task is still
active surfaces to the channel as the "still working" notice.

**"Starting fresh" and resets.** A reply in a thread with no binding gets the channel's
starting-fresh notice and starts a new instance — this is also what a thread from before the
cut-over to kagent API v2 sees: its earlier conversation is gone and the turn starts a new one.
A binding whose instance the controller no longer has (`GetAgentInstance` → not found) is
dropped the same way. The corrupt-session recovery (`ResetSession`) deletes the instance with
`DeleteAgentInstance` and drops the binding, so the next turn creates a fresh instance.

## Human-in-the-loop

A tool bound with `requireApproval: true` (and the agent's built-in `ask_user` question tool)
pauses the task at `input-required`. The pause only carries a decidable request when the
client asked for kagent's HITL extension, so the gateway requests
`https://kagent.dev/extensions/hitl/v1` on every A2A call (the `A2A-Extensions` service
parameter, gRPC metadata `a2a-extensions`). The paused task's status message then carries a
typed payload under the extension URI in its metadata:

- `tool_approval_request` — `hint`, `tools[]` (`id`, `call_id`, `name`, `args`), optionally
  `nested` (a request propagated from a sub-agent, decided on the child's tools);
- `ask_user_request` — `id`, `questions[]` (`question`, `choices`, `multiple`).

`pkg/channels` parses that payload into the `HitlPrompt` the channels render (Approve/Deny,
a choice widget, a form), and the message's text part is the agent's hint. The channel's
answer is a `HitlDecision`; the gateway turns it into the matching response — one approval per
requested tool (all approved or all rejected together, the reason on a rejection), or the
positional `ask_user_response` under the request's correlation id — by reading the paused task
(`GetTask`) and building the payload against the request it is paused on. The decision travels
as the HITL payload of a message that **carries the paused task's id**, so the task resumes in
place and nothing is left waiting at `input-required`.

## Stop

`/stop` (and a web client closing its stream) cancels the gateway-side turn and then calls
`CancelTask` on the running task, so the agent stops working server-side; `GetTask` reports the
task canceled. A task paused on a prompt is not canceled by a stop — it is resolved by a
decision (the Slack `/stop` on a paused thread is routed as a rejection).

## Configuration

| Value | Flag / env | Meaning |
|---|---|---|
| `a2a.enabled` | `--a2a-enabled` / `KLAUS_GATEWAY_A2A_ENABLED` | Turn the kagent client on |
| `a2a.url` | `--a2a-url` / `KLAUS_GATEWAY_A2A_URL` | `grpc://host:port` (h2c) or `grpcs://host:port` (TLS) — the controller through agentgateway |
| `a2a.namespace` | `--a2a-namespace` / `KLAUS_GATEWAY_A2A_NAMESPACE` | Namespace whose AgentTemplates are served (default `kagent`) |
| `a2a.defaultAgent` | `--a2a-default-agent` / `KLAUS_GATEWAY_A2A_DEFAULT_AGENT` | Template a turn runs on when the channel names none |
| `a2a.caSecret`, `a2a.caFile` | `--a2a-ca-file` / `KLAUS_GATEWAY_A2A_CA_FILE` | CA bundle trusted for a `grpcs://` target besides the system roots (`caSecret`: a Secret with `ca.crt`, mounted by the chart) |
| `a2a.fallbackIconUrlTemplate` | `--a2a-fallback-icon-url-template` | Icon when the template has no icon annotation; `{agent}` = technical name |
| `a2a.saToken`, `a2a.tokenPath` | `--a2a-token-path` | Projected ServiceAccount token for the Klaus-instance paths; never presented to the controller |

Requirements: a kagent API v2 controller (`kagent.dev/v1alpha3`, the `lf.a2a.v1` and
`kagent.api.v1alpha1` gRPC services on one port) behind a gRPC-capable route that validates the
person's token and preserves the two metadata entries above. Conversations from before the
cut-over are not migrated: the first reply in such a thread gets the starting-fresh notice.

## Verification

The headless proof runs the gateway from a branch build against a lab controller: the `web`
channel with a lab user's Dex id_token forwarded, `GET /web/agents` for discovery, one streamed
turn attributed to the person at the MCP gateway, one HITL round trip on a template whose
tool binding carries `requireApproval: true` (the task reaches `input-required`, the decision
resumes it, `GetTask` shows nothing left waiting), a stop that `GetTask` reports as canceled
followed by a further turn, and a gateway restart on the bolt store that continues the same
`AgentInstance`. Slack itself is verified on a platform installation.
