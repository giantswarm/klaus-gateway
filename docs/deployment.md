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

## Routing store

The routing table maps `(channel, channelID, userID, threadID)` to a Klaus instance name.
Choose the backend that matches your deployment:

| Store       | Helm value         | Persistent | Cluster-backed | Notes                              |
|-------------|-------------------|------------|----------------|------------------------------------|
| `memory`    | `routing.store: memory`    | no  | no  | Default; state lost on restart     |
| `bolt`      | `routing.store: bolt`      | yes | no  | Local file; set `routing.boltPath` |
| `configmap` | `routing.store: configmap` | yes | yes | One ConfigMap per namespace        |
| `crd`       | `routing.store: crd`       | yes | yes | One `ChannelRoute` CR per conversation; requires `controller.enabled` |

### ChannelRoute CRD

When `routing.store: crd`, the gateway reads and writes `ChannelRoute` custom resources
(`routing.giantswarm.io/v1alpha1`). The CRD is rendered by the chart when `crd.install: true`
(default).

Enable the embedded controller to watch `ChannelRoute` CRs and update their status conditions:

```yaml
routing:
  store: crd
controller:
  enabled: true
```

The controller requires cluster-scoped RBAC to list/watch/patch `ChannelRoute` resources; the
chart's `templates/rbac.yaml` grants this when `controller.enabled: true`.

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
