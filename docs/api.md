# klaus-gateway API

klaus-gateway exposes the Slack adapter's own routes, the muster account-linking routes and the
team-review endpoint on its public mux, and health, readiness and metrics on the admin mux. The
Slack surface is documented in [channels-slack.md](channels-slack.md); this page covers the rest.

## Team-review endpoint

Through this surface a manager posts an ask into a team's Slack channel — the change spelled out,
the pull requests it lands as, an *Approve* button, a *Deny* button when the manager names a deny
tool, a link — or a notice that asks for nothing, and later the outcome of the action into the
review's thread. It is the inbound side of the [team review](slack-hitl-surface.md#10-team-review-a-managers-ask-to-a-team)
prompt. Mounted when `--reviews-enabled` is set (chart: `reviews.enabled`); requires the Slack
adapter and OBO account linking, since a click runs as the linked person.

### Authentication: the caller's own ServiceAccount

The caller sends the projected Kubernetes ServiceAccount token it already has:

```http
Authorization: Bearer <projected ServiceAccount token, audience klaus-gateway>
```

The gateway verifies it through the `TokenReview` API for its audience (`--reviews-audience`,
default `klaus-gateway`) and admits only the ServiceAccounts listed in
`--reviews-allowed-callers` (`system:serviceaccount:<namespace>:<name>`). No personal token, no
shared secret: the API server is the verifier. The caller projects its token for the audience:

```yaml
volumes:
- name: klaus-gateway-token
  projected:
    sources:
    - serviceAccountToken:
        audience: klaus-gateway
        expirationSeconds: 600
        path: token
```

| Situation | Status |
|---|---|
| No bearer token, or one the API server does not vouch for | 401 (`WWW-Authenticate: Bearer`) |
| ServiceAccount not in the allow-list | 403 |
| API server unreachable for the review | 503 |
| Invalid body (see below) | 400 |
| A result for a review the gateway does not hold (unknown, or past its seven days) | 404 |
| Slack refused the post | 502 |

### `POST /reviews`

```json
{
  "team": "team-bumblebee",
  "channel": "C0123ABCDE",
  "text": "*Enable* `agent-platform` on two installations for <@U…>: …",
  "actor": "alex@example.com",
  "pullRequests": [
    "https://github.com/giantswarm/a-configs/pull/12",
    "https://github.com/giantswarm/b-management-clusters/pull/7"
  ],
  "link": "https://github.com/giantswarm/platform-manager/actions/runs/4242",
  "approve": {
    "tool": "x_giantswarm-platform-manager_approve_action",
    "arguments": {"action": "a1"}
  },
  "deny": {
    "tool": "x_giantswarm-platform-manager_deny_action",
    "arguments": {"action": "a1"}
  },
  "noticeChannel": "C0456FGHIJ"
}
```

- `team` — the GitHub team (slug) whose linked members may decide. Required.
- `channel` — the Slack channel **ID** (`C…`), not a name: the message is rewritten in place after
  the decision and `chat.update` needs the ID. Required.
- `text` — the change spelled out, Slack mrkdwn, at most 3000 characters. The caller is a trusted
  service, so the text is rendered as written. Required.
- `actor` — the person whose action the review decides, as the gateway knows linked people: the
  **email** of the identity their Slack account was linked to (the gateway verifies the Slack
  profile's email against it at link time; it holds no GitHub login). Their own *Approve* is
  refused with a status line under the buttons — a second person decides; their *Deny* withdraws
  the action. The manager's own author check under the person's GitHub grant stands regardless.
  Optional.
- `pullRequests` — the pull requests the change lands as, one http(s) URL each, at most 25,
  rendered as one link per line named `owner/repo#n` for GitHub's URL shape. Optional.
- `link` — an http(s) URL, rendered as a button for anything the decision buttons do not cover,
  labelled by what it points to: *Open PR* (`/pull/<n>`), *Open run* (`/actions/runs/<id>`),
  *Open issue* (`/issues/<n>`), *Open repository* (a bare repository page), *Open link* otherwise.
  Optional.
- `approve.tool`, `approve.arguments` — the muster tool a member's click calls, verbatim, under
  that member's identity (their own token; muster and the tool see the person). `tool` required.
  The gateway reaches it through muster's `call_tool` meta-tool — the way every aggregated
  `x_<server>_<tool>` is exposed — and reads the tool's own `isError` verdict out of the envelope
  the meta-tool returns.
- `deny.tool`, `deny.arguments` — when given, the message carries a *Deny* button. The click opens
  a modal with one required box; the tool is called as the member with the arguments plus
  `"reason": "<what they typed>"` (up to 500 characters). Optional; `arguments` without a `tool`
  is refused.
- `noticeChannel` — a second Slack channel ID that receives the same text, the pull requests and
  the link as a notice without buttons — the Account Engineers' channel when a customer
  installation is a target. Posted before the review, so a channel Slack refuses fails the request
  before anything with buttons is up. Must differ from `channel`. Optional.

Unknown fields are refused. Response `201`:

```json
{"id": "8f3c2d…", "channel": "C0123ABCDE", "ts": "1726512345.000100", "notice_ts": "1726512344.000200"}
```

`notice_ts` locates the notice when `noticeChannel` was given.
`id` is the gateway's handle on the review; a click on the message is resolved against it, and
`POST /reviews/{id}/results` posts into its thread. The
review — the ask, its message, the decision state and the status line — is a record in the
gateway's routing store (`--store`, chart `routing.store`) for seven days, so on `valkey` (what
installations run; one key per review, `klaus-gateway:review:<id>`, the seven days as the key's
expiry) and on `bolt` a review posted before a gateway restart is approved by a click after it,
and a click on a review somebody approved before the restart is told who decided. On `memory`
the records die with the process, and a click on a dropped review rewrites the message to say it
expired. A review past its seven days reads the same, and the manager posts the ask again. A
store that does not answer a click is not an expiry: the clicker is told to click again in a
moment, and the message keeps its buttons.

One decision closes a review across replicas too: the click's claim on the record is a
compare-and-set on the Valkey store, so of two clicks only one calls the tool and the other is
told who decided (and how: approved or denied). A claim left without an outcome for five minutes
— a gateway that died during the tool call, which the muster client bounds to a minute — is
taken over by the next click instead of holding the review for the rest of its seven days. A
*Deny* click holds nothing: the modal it opens claims the review on submission.

The tool's answer decides what the click did. A success closes the review; what the tool said is
shown to the team under the outcome — a plain text as written, a JSON object by its `message`
field (a tool that answers agents with structured data puts the sentence for the channel there),
nothing of structured data without one; a denial also shows the typed reason. An error result is
a refusal (the manager finding the person outside the team, or the author of the change) and is
written once, as a status line under the buttons naming the clicker, the decision and the reason,
where the clicker and the team both read it; the review stays open.
muster's sign-in challenge — the tool's server holds no grant for the person yet — is not a
refusal: the clicker gets a *Connect <server>* button, and when the gateway has a public base URL
(`obo.callbackBaseURL`) and muster's `oauth.mcpClient.postLoginRedirectAllowlist` admits
`<base>/connectors/complete`, the sign-in lands back on the gateway and the decision — the
approval, or the denial with its reason — is submitted again as the person without a second click.

### `POST /reviews/{id}/results`

The outcome of the action the review approved — the pull requests merged, the rollout, the probes
green or the one that failed — posted as a reply in the review message's thread, so one thread
carries the whole action. Post as many as the action has stages.

```json
{"text": "✅ Merged and rolled out on both installations; every probe green.", "link": "https://github.com/giantswarm/platform-manager/actions/runs/4242"}
```

- `text` — Slack mrkdwn, at most 3000 characters, rendered as written. Required.
- `link` — an http(s) URL, rendered as a context line labelled by its kind (see `link` above).
  Optional.

`{id}` is the `id` `POST /reviews` returned. Response `201` with `id`, `channel` and the reply's
`ts`; `404` for a review the gateway does not hold — unknown, or past its seven days (on
`routing.store: memory`, a gateway restart).

### `POST /notices`

Same authentication and fields as `POST /reviews` except `actor`, `approve`, `deny` and
`noticeChannel`; renders without buttons, the pull requests as links and the link as a context
line. Response `201` with `channel` and `ts`.

## Admin surface

Served on the admin port (default `:8081`):

- `GET /healthz` -- liveness
- `GET /readyz`  -- readiness (probes the routing store)
- `GET /metrics` -- Prometheus scrape endpoint: the public mux's request counter and latency,
  `klaus_gateway_turn_total{channel,outcome}` and `klaus_gateway_turn_phase_seconds{channel,phase}`
  for the channel turns (see [deployment.md](deployment.md#observability))
