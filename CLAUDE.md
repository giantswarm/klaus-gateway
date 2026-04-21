# CLAUDE.md -- klaus-gateway

Project context for AI coding agents working in this repo.

## What this is

`klaus-gateway` is the channel and routing front door for [Klaus](https://github.com/giantswarm/klaus)
instances. It is **not** the LLM/MCP data plane — that is
[agentgateway](https://github.com/agentgateway/agentgateway). Keep these roles separate when
reading or writing code:

- **agentgateway** — proxies `/v1/*`, `/mcp`, A2A; speaks OpenAI / MCP / A2A natively;
  enforces JWT/Cedar policy.
- **klaus-gateway** — exposes channel adapters (Slack, web, CLI), maps
  `(channel, channelID, userID, threadID)` to a Klaus instance, creates instances on demand,
  and forwards LLM traffic through agentgateway.

## Stack

- Go 1.26
- HTTP: `net/http` + `chi` router (`github.com/go-chi/chi/v5`)
- Logging: `log/slog`
- OTel: `go.opentelemetry.io/otel` (OTLP gRPC)
- CRD controller: `sigs.k8s.io/controller-runtime`
- CI: giantswarm `architect` orb. Multi-arch (amd64+arm64) Docker images.

## Package layout

```
cmd/klaus-gateway/      entrypoint; wires stores, lifecycle drivers, adapters, server
pkg/api/                OpenAI-compat front door (/v1/{instance}/...)
pkg/api/v1alpha1/       ChannelRoute CRD types (routing.giantswarm.io/v1alpha1)
pkg/channels/           ChannelAdapter interface + Gateway facade
pkg/channels/web/       web channel adapter (/web/*)
pkg/channels/slack/     Slack channel adapter (/channels/slack/*); Events API + Socket Mode
pkg/channels/cli/       CLI channel adapter (/cli/v1/*)
pkg/instance/           HTTP client for Klaus instances + SSE helpers
pkg/lifecycle/          lifecycle.Manager interface + drivers
pkg/lifecycle/klausctl/ calls klausctl CLI (local dev)
pkg/lifecycle/operator/ calls Klaus Operator MCP tools (cluster)
pkg/lifecycle/static/   fixed instance map (compose harness / CI)
pkg/routing/            routing table
pkg/routing/store/      Store interface + four backends (memory, bolt, configmap, crd)
pkg/server/             http.Server wiring, middleware, admin mux
pkg/upstream/           agentgateway upstream URL rewriter
pkg/observability/      OTel traces + Prometheus metrics
internal/config/        env-var + flag config (KLAUS_GATEWAY_* prefix)
internal/controller/    ChannelRoute controller-runtime reconciler
internal/version/       ldflags-injected version metadata
helm/klaus-gateway/     Helm chart
deploy/docker-compose.yml     compose smoke harness
deploy/agentgateway/    standalone agentgateway config for the harness
deploy/slack/manifest.yaml    Slack app manifest
```

## Routing stores

Four backends are supported (set via `--store` / `KLAUS_GATEWAY_STORE`):

| Store       | Value        | Persistent | Cluster-backed | Notes                                       |
|-------------|-------------|------------|----------------|---------------------------------------------|
| Memory      | `memory`    | no         | no             | Default; state lost on restart              |
| Bolt        | `bolt`      | yes        | no             | Local file; path via `--bolt-path`          |
| ConfigMap   | `configmap` | yes        | yes            | One `ConfigMap` per namespace               |
| CRD         | `crd`       | yes        | yes            | One `ChannelRoute` CR per conversation; requires `--controller` |

When `--store=crd --controller=true` the embedded `controller-runtime` manager is started
in-process and watches `ChannelRoute` CRs to update their status conditions.

## Lifecycle drivers

Three drivers are supported (set via `--driver` / `KLAUS_GATEWAY_DRIVER`):

- `klausctl` — shells out to `klausctl` to create/list instances. Default for local dev.
- `operator` — calls Klaus Operator via MCP (`--operator-mcp-url`). Used in cluster.
- `static` — maps a fixed comma-separated `name=baseURL` list. Used in the compose harness.

## Local testing

**Developer path** (preferred): `klausctl gateway start` — spins up `klaus-gateway`,
`agentgateway`, and a Klaus instance with your LLM key.

**Compose harness** (CI / contributor smoke test):

```bash
make e2e-local        # build + run end-to-end
make e2e-local-up     # bring up in background
make e2e-local-down   # tear down
```

The harness uses the `static` driver with `test-instance` mapped to `klaus-instance-stub`,
a tiny Go server that mimics the Klaus HTTP surface.

## Conventions

- Module path: `github.com/giantswarm/klaus-gateway`
- Helm chart name: `klaus-gateway` (no `-app` suffix; this is a service repo)
- Team: `bumblebee` (annotation `application.giantswarm.io/team: bumblebee`)
- Container image: `gsoci.azurecr.io/giantswarm/klaus-gateway`
- Branch naming: `klaus/agent/<timestamp>` for agent-authored branches
- Versioning: build metadata via `-ldflags` into `pkg/project`
- Comments: explain non-obvious intent only; don't narrate code

## Gitleaks

The repo runs gitleaks on every PR. Do not include strings beginning with
`Slack bot`, `Slack app-level`, or `Slack user` anywhere in committed content, even in test
fixtures or documentation examples — they match the Slack token patterns and
will fail the scan.

## Reference repos in the lab

- `giantswarm/klaus` — agent binary; `klaus-gateway` proxies traffic to its `/v1/*` and `/mcp` endpoints.
- `giantswarm/klaus-operator` — exposes MCP tools (`create_instance`, `list_instances`, …) used by the cluster lifecycle driver.
- `giantswarm/klausctl` — local lifecycle equivalent; `klaus-gateway` calls it for `--driver=klausctl` mode.
- `giantswarm/lab` — webapp whose `chatproxy.go` talks to `klaus-gateway` via the web channel adapter.
- `agentgateway/agentgateway` (Linux Foundation) — the data plane; consumed via OCI image and `gateway.networking.k8s.io` CRDs.

Architecture and ADRs: [`teemow/klaus-lab`](https://github.com/teemow/klaus-lab) under
`architecture/klaus-gateway.md` and `decisions/`.

## Build / test

```bash
go build ./...
go test ./...
docker build -t klaus-gateway:dev .
make lint        # golangci-lint with gosec + goconst
make helm-test   # helm lint + template render assertions
```

## CI

CircleCI via the `architect` orb: `go-build`, `push-to-registries-multiarch`,
`push-to-app-catalog`. Branch builds run amd64 only; tag builds (`v*`) run full multi-arch.
