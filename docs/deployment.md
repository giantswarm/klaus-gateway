# Deploying klaus-gateway

`klaus-gateway` ships as a Helm chart at `helm/klaus-gateway/`. The chart always renders the
gateway itself (`Deployment`, `Service`, `ServiceAccount`, `ServiceMonitor`). It optionally
renders Kubernetes Gateway API + upstream [agentgateway](https://github.com/agentgateway/agentgateway)
resources so the gateway sits behind an agentgateway data plane.

## Modes

| Mode                                  | When to use                                                                 |
|---------------------------------------|-----------------------------------------------------------------------------|
| Cluster mode **without** agentgateway | Dev clusters, CI clusters, or any cluster that does not run agentgateway.   |
| Cluster mode **with** agentgateway    | Production clusters (e.g. spidertron) where policy/authn live on the edge. |

The agentgateway block is off by default, so the chart stays installable on clusters that do
not have the `agentgateway.dev` CRDs.

## Cluster mode (without agentgateway)

Plain install — no Gateway API resources, no agentgateway CRDs required:

```bash
helm upgrade --install klaus-gateway helm/klaus-gateway \
  --namespace klaus-gateway --create-namespace
```

Traffic lands directly on the `klaus-gateway` `Service` (port 80, container port
`server.port`). Expose it with whatever mechanism you already use (`Ingress`,
`LoadBalancer` service, port-forward, etc.).

## Cluster mode (with agentgateway)

### Prerequisites

1. Install the Kubernetes Gateway API standard CRDs (`gateway.networking.k8s.io/v1`):

    ```bash
    kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.5.1/standard-install.yaml
    ```

2. Install upstream agentgateway (installs the `AgentgatewayPolicy` and `AgentgatewayBackend`
   CRDs and the controller):

    ```bash
    helm upgrade --install agentgateway \
      oci://ghcr.io/agentgateway/charts/agentgateway \
      --version v1.1.0 \
      --namespace agentgateway --create-namespace
    ```

   These templates target agentgateway **v1.1.0** (`agentgateway.dev/v1alpha1`). The pinned
   version is recorded in `Chart.yaml` under the
   `klaus-gateway.giantswarm.io/agentgateway-version` annotation.

### Install

Enable the agentgateway block to render a `Gateway`, an `HTTPRoute`, and optionally an
`AgentgatewayPolicy` and example `AgentgatewayBackend`:

```yaml
# values-cluster.yaml
agentgateway:
  enabled: true
  gatewayClassName: agentgateway
  gateway:
    create: true
    listeners:
    - name: http
      port: 80
      protocol: HTTP
      allowedRoutes:
        namespaces:
          from: Same
  routes:
    chatAndMcp:
      enabled: true
      prefixes:
      - /v1/
      - /mcp
  policy:
    enabled: true
    jwt:
      enabled: true
      mode: Strict
      issuer: https://auth.example.com/
      audiences:
      - klaus
      jwks:
        mode: remote
        remote:
          jwksPath: /.well-known/jwks.json
          cacheDuration: 5m
          backendRef:
            kind: Service
            name: oidc-jwks
            namespace: auth
            port: 443
```

```bash
helm upgrade --install klaus-gateway helm/klaus-gateway \
  --namespace klaus-gateway --create-namespace \
  -f values-cluster.yaml
```

### What gets rendered

When `agentgateway.enabled=true`:

| Resource                                          | Template                                  | Gated by                                    |
|---------------------------------------------------|-------------------------------------------|---------------------------------------------|
| `gateway.networking.k8s.io/v1 Gateway`            | `templates/agentgateway-gateway.yaml`     | `agentgateway.gateway.create`               |
| `gateway.networking.k8s.io/v1 HTTPRoute`          | `templates/agentgateway-httproute.yaml`   | `agentgateway.routes.chatAndMcp.enabled`    |
| `agentgateway.dev/v1alpha1 AgentgatewayPolicy`    | `templates/agentgateway-policy.yaml`      | `agentgateway.policy.enabled`               |
| `agentgateway.dev/v1alpha1 AgentgatewayBackend`   | `templates/agentgateway-backend.yaml`     | `agentgateway.backendsExample.enabled`      |

The example `AgentgatewayBackend` is a placeholder pointing at a single Klaus instance
`Service`. In production each instance `Backend` is created by
[Klaus Operator](https://github.com/giantswarm/klaus-operator) at runtime; leave
`backendsExample.enabled=false` there.

### Attaching to an externally-managed Gateway

If a shared `Gateway` already exists (e.g. owned by the platform team), set
`agentgateway.gateway.create=false` and point `parentRefs` at it:

```yaml
agentgateway:
  enabled: true
  gateway:
    create: false
    parentRefs:
    - name: platform-gateway
      namespace: gateway-system
```

The `HTTPRoute` and `AgentgatewayPolicy` will attach to that Gateway instead of rendering
a new one.

### JWT validation

The chart renders `spec.traffic.jwtAuthentication` on the `AgentgatewayPolicy`. Both JWKS
modes are supported:

- **remote** (default) — agentgateway fetches the JWKS via a `Service` or
  `AgentgatewayBackend` backend ref. Requires
  `agentgateway.policy.jwt.jwks.remote.backendRef.name`.
- **inline** — ship a literal JWKS JSON document in the policy. Set
  `agentgateway.policy.jwt.jwks.mode=inline` and provide
  `agentgateway.policy.jwt.jwks.inline`.

### Cedar policies

Cedar is **not** part of agentgateway v1.1.0. Once upstream lands Cedar, the chart will
grow a `policy.cedar` block and a `templates/agentgateway-cedar-policy.yaml` ConfigMap.

## kagent (agent conversations)

With `a2a.enabled: true` the gateway runs channel turns on a kagent API v2 controller through
the platform's agentgateway: A2A v1 over gRPC, the AgentTemplate roster over kagent's gRPC
services, one AgentInstance per thread. The full model is in [kagent-a2a.md](kagent-a2a.md).

```yaml
a2a:
  enabled: true
  # in-cluster: plaintext h2c to the agentgateway Service
  url: grpc://agentgateway.agent-platform.svc.cluster.local:8080
  # or public: TLS to the kagent hostname (add caSecret for a private CA)
  # url: grpcs://agentgateway.<domain>:443
  # caSecret: kagent-ca        # Secret with key ca.crt
  namespace: kagent            # the AgentTemplates served
  defaultAgent: sre-agent
```

The gateway speaks to the controller only as the person behind the turn (their Dex id_token),
so the route's JWT policy validates one issuer and no ServiceAccount token is presented to it.
The route must carry native gRPC over HTTP/2 and preserve the `authorization` and
`x-kagent-agent-instance-id` metadata. Thread bindings live in the routing store, so a
persistent store (`valkey`) keeps conversations across restarts — and lets a
restarted gateway pick up the turns its predecessor left running, see
[Shutdown and restarts](#shutdown-and-restarts).

## OBO link store

With Slack on-behalf-of linking (`obo.enabled`), the gateway keeps one record per linked Slack
user (muster identity, the encrypted refresh token, the cached id_token). `obo.store` selects
where those records live; both backends seal every record with `store-key` (AES-256-GCM):

| Store    | Helm value            | Volume | Node-bound | Notes                                                            |
|----------|-----------------------|--------|------------|------------------------------------------------------------------|
| `bolt`   | `obo.store: bolt`     | yes    | yes        | Default. File at `obo.storePath`; `obo.persistence` picks emptyDir or a RWO PVC (then `Recreate`) |
| `secret` | `obo.store: secret`   | no     | no         | One Secret `<release>-obo-links`; Role/RoleBinding rendered; replicas can share it; imports the bolt file on first start |

`UPGRADE.md` describes the move from the volume to the Secret.

Both backends can fail a call — the Secret backend on any apiserver hiccup (a restart, a
`resourceVersion` conflict past the retries, the 10 s call timeout), the bolt file on a full
disk — and the gateway keeps a process-local copy of every link it has read or written so a
failure never costs a person their sign-in:

- A refresh token muster has already rotated is kept in memory when the store refuses the
  write, the write is retried in the background (2 s, doubling to 30 s) and once more on
  shutdown, and the next refresh uses the rotated token. The log line
  `link store write failed, keeping the link in memory and retrying` marks the failure,
  `link store write retry succeeded` the recovery; only a pod that dies before the retry lands
  loses the rotation, and that person signs in again.
- A store that fails to read serves the link the gateway already knows (`link store read
  failed, serving the link this process knows`). A person it has never seen is not treated as
  unlinked: in Slack they get the transient "couldn't refresh your sign-in" notice, not the
  sign-in prompt, and `/login` answers the same way. `/logout` reports a sign-out the store
  refused instead of confirming it.
- Reads are served from the copy for 30 s, so a Slack turn costs one Secret read rather than
  one per lookup. Before a link is dropped on `invalid_grant` the store is re-read, so a token
  rotated by another writer (a second replica, the import) is retried rather than burned.

## Routing store

The routing table maps `(channel, channelID, userID, threadID)` to a Klaus instance name, or a
thread to the kagent AgentInstance that holds its conversation, together with the record of the
task in flight on that thread (delivered after a restart, see
[Shutdown and restarts](#shutdown-and-restarts)). Choose the backend that matches your deployment:

| Store       | Helm value         | Persistent | Cluster-backed | Notes                              |
|-------------|-------------------|------------|----------------|------------------------------------|
| `memory`    | `routing.store: memory`    | no  | no  | Default; state lost on restart     |
| `valkey`    | `routing.store: valkey`    | yes | yes | For installations. One key per thread in a Valkey server; set `routing.valkey.url` and the password Secret |
| `bolt`      | `routing.store: bolt`      | file | no | Local file; set `routing.boltPath`. Durable only inside a mounted volume, which the chart does not provide |

### Valkey

`routing.store: valkey` keeps one key per thread (`klaus-gateway:route:` + the serialised routing
key, so a channel's entries share a prefix) with the JSON entry as its value; an entry with a TTL
expires server-side. The gateway needs no volume and no API-server access, and replicas can
share the table. The agent platform runs a Valkey for muster's token store (`muster-valkey:6379`,
password under `valkey-password` in the platform Secret), which the gateway can share:

```yaml
routing:
  store: valkey
  valkey:
    url: muster-valkey:6379
    existingSecret: agent-platform-secrets   # the platform's shared Secret
    passwordKey: valkey-password
    # username: ""        # ACL user; empty is the default user
    # tls: {enabled: false, serverName: ""}
    # keyPrefix: ""       # klaus-gateway:route: — set it when two gateways share one server
    # timeout: "2s"
```

The chart passes the password to the pod as `KLAUS_GATEWAY_VALKEY_PASSWORD` from the Secret; the
binary also takes `--valkey-password-file` for a mounted file. Every dial and command is bounded by
`routing.valkey.timeout` (default 2 s): while Valkey is unreachable a turn fails after that long
with a clear error in the thread, the pod's readiness probe (a `PING`) fails, and both recover with
the server — no restart. Under a Cilium network policy the gateway pod needs egress to the Valkey
pods on 6379 (the agent platform's connectivity chart renders it).

## Lifecycle driver

The lifecycle driver creates Klaus instances on demand when a route miss occurs.

| Driver     | Helm value              | When to use                            |
|------------|-------------------------|----------------------------------------|
| `operator` | `lifecycle.driver: operator` | Cluster deployments; calls Klaus Operator MCP tools |
| `klausctl` | `lifecycle.driver: klausctl` | Local / single-node; calls `klausctl` CLI |
| `static`   | `lifecycle.driver: static`  | Compose harness or fixed single-instance |

For the `operator` driver, set:

```yaml
lifecycle:
  driver: operator
  operatorMCPURL: http://klaus-operator.default.svc:8090
  operatorMCPToken: ""  # optional bearer token
```

## Channel configuration

### Slack

The Slack adapter is disabled by default. To enable it, create a Kubernetes `Secret` with
the Slack credentials and reference it in values:

```bash
kubectl create secret generic slack-credentials \
  --namespace klaus-gateway \
  --from-literal=bot-token='<bot OAuth token>' \
  --from-literal=signing-secret='<signing secret>' \
  --from-literal=app-token='<app-level token>'   # socketmode only
```

```yaml
slack:
  enabled: true
  mode: events      # "events" (Events API webhook) or "socketmode" (development)
  secretName: slack-credentials
```

The secret values are injected as `SLACK_BOT_TOKEN`, `SLACK_SIGNING_SECRET`, and
`SLACK_APP_TOKEN` environment variables respectively.

For Events API mode, set the Request URL in your Slack app to
`https://<your-domain>/channels/slack/events`. Use `deploy/slack/manifest.yaml` to create
and configure the Slack app in one step.

See [docs/channels-slack.md](channels-slack.md) for the full Slack setup guide.

### CLI

The CLI adapter is disabled by default:

```yaml
cli:
  enabled: true

# Optionally expose /cli/v1/* through agentgateway:
agentgateway:
  routes:
    cli:
      enabled: true
      prefixes:
      - /cli/v1/
```

See [docs/channels-cli.md](channels-cli.md) for usage.

## Shutdown and restarts

On `SIGTERM` the gateway drains its HTTP servers (up to 15 s), then stops the channel adapters
(up to 15 s, one budget for all of them), and only then closes the kagent client and the
stores. The adapter stop is where a Slack turn cut short posts its restart notice, clears its
progress reaction and collapses its status ticker; the task itself is left running at the
controller, and its id stays on the thread's routing-store binding so the next process can
resubscribe to it and deliver the answer ([channels-slack.md](channels-slack.md#restarts-and-stop)).

`terminationGracePeriodSeconds` (default `45`) has to cover both windows with some margin for
the closes; below the drain plus the stop, the kubelet kills the pod before the notice goes
out and the thread is left with a frozen ticker. The recovery of left-running turns needs a
routing store that outlives the pod: `routing.store: memory` (the chart default) forgets the
binding and the task with it. Installations with a Slack channel should run `valkey` (see
[Valkey](#valkey)); `bolt` only counts when `routing.boltPath`
lies inside a mounted volume.

## Observability

The admin port serves `GET /metrics` (Prometheus; `serviceMonitor.enabled` renders the
ServiceMonitor). Beside the public mux's `klaus_gateway_requests_total` /
`klaus_gateway_request_duration_seconds`, every channel turn (Slack, web, CLI) feeds:

- `klaus_gateway_turn_total{channel, outcome}` -- turns that ended, by outcome: `completed`,
  `input_required` (paused on a prompt), `canceled` (`/stop`, the stop button, a closed stream),
  `shutdown` (the gateway's restart cut it short), `timeout` (the 30-minute turn deadline),
  `failed` (the task failed or the stream broke), `render_failed` (the task completed but the
  channel refused part of the answer), `resolve_failed` / `send_failed` (the turn died before it
  was sent).
- `klaus_gateway_turn_phase_seconds{channel, phase}` -- one histogram per phase of the turn's
  timeline, measured from the moment the channel received the message: the marks `dispatch`
  (admission, identity and agent resolved), `first_event` (the controller's first A2A event),
  `first_text` (the first text of the answer), `task_done` (the task's terminal state),
  `stream_end` (the A2A stream closed), `final_flush` (the last edit of the answer in the
  channel) and `total`; and the durations of the steps `token_mint` (the person's muster token,
  ~0 on a cache hit, a round trip to muster on a refresh), `roster` (agent resolution),
  `intro_post` (the launch announcement of a new conversation) and `create_instance` (the
  controller's `CreateAgentInstance` on a thread's first turn). Buckets run from 5 ms to 5 min.
  "Message received → answer landed" is `phase="final_flush"`; p50/p95 of it is the panel to
  watch.

The same numbers are on the `turn_complete` log record every turn ends with (see
[channels-slack.md](channels-slack.md#records)), as `<phase>_ms` fields next to the outcome, the
task id, the tool-call count and the streamed characters, so a single turn can be read from the
log while the histograms show the population.

Traces: every turn is one trace. The gateway opens a `<channel>.turn` span when the message
arrives; the A2A calls to the kagent controller (`otelgrpc`), the Slack Web API calls
(`slack.<method>`) and a muster token refresh (`musterlink.<endpoint>`) are client spans under it,
and the A2A call carries the W3C `traceparent`, so the controller's own `SendStreamingMessage`
trace -- and, under it, the actor's spans -- continues the gateway's instead of starting a new
one. The `turn_dispatch` and `turn_complete` records carry the `trace_id`. Export is off until
`observability.otlpEndpoint` names an OTLP gRPC collector (a URL whose scheme decides TLS, such
as `http://otlp-gateway.kube-system.svc:4317`, or a bare `host:port` in plaintext);
`observability.otlpHeaders` adds headers to every export, such as the tenant of a multi-tenant
gateway:

```yaml
observability:
  otlpEndpoint: http://otlp-gateway.kube-system.svc:4317
  otlpHeaders:
    X-Scope-OrgID: giantswarm
```

The agent platform's meta chart sets both by default (following its observability answer, the
way kagent's exporters do) and renders the gateway's egress rule to the collector. Without an
endpoint spans are still created -- the trace ids in the records are real and the `traceparent`
still reaches the controller -- but nothing is exported. Sampling is `ParentBased(AlwaysSample)`:
every turn is exported; the traffic is turns, not requests.

## Values reference

See `helm/klaus-gateway/values.yaml` for the full set. The agentgateway block is validated by
`helm/klaus-gateway/values.schema.json`; `helm install` and `helm upgrade` reject unknown
fields or wrong types.

## Local checks

```bash
make helm-test
```

Runs `helm lint` and `helm template` in both agentgateway-disabled and -enabled modes and
asserts the expected kinds are (or are not) present.
