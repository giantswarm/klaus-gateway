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
main.go                 entrypoint; wires stores, lifecycle drivers, adapters, server
pkg/a2a/                kagent API v2 client: A2A v1 over gRPC turns, AgentTemplate roster, AgentInstance per thread, HITL payloads
pkg/kagent/gen/         generated kagent.api.v1alpha1 gRPC stubs (make generate-kagent; pin in its README)
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
pkg/routing/store/      Store interface + five backends (memory, valkey, crd, bolt, configmap)
pkg/auth/musterlink/    Slack OBO: muster account linking + the link Store (memory, bolt file, Kubernetes Secret)
pkg/server/             http.Server wiring, middleware, admin mux
pkg/upstream/           agentgateway upstream URL rewriter
pkg/observability/      OTel traces + Prometheus metrics
pkg/project/            build identifiers: ldflags target for version, git SHA and build timestamp; version falls back to the Go build info
internal/config/        env-var + flag config (KLAUS_GATEWAY_* prefix)
internal/controller/    ChannelRoute controller-runtime reconciler
internal/version/       re-exports pkg/project for the rest of the code
helm/klaus-gateway/     Helm chart
hack/kagent-proto/      kagent protos copied from giantswarm/kagent-upstream; input of make generate-kagent
hack/helm-template-tests      renders the chart with agentgateway off and on and asserts the expected kinds
hack/wait-for, hack/smoke-completion   scripts of the compose smoke harness
tests/                  chart tests CI runs on a kind cluster (app-test-suite): tests/ats/, tests/test-values.yaml
deploy/docker-compose.yml     compose smoke harness
deploy/klaus-gateway.Dockerfile   image build for the harness (public base image; the production Dockerfile copies a prebuilt binary)
deploy/klaus-instance-stub/   tiny Go server that mimics the Klaus HTTP surface for the harness
deploy/agentgateway/    standalone agentgateway config, shared by the harness and klausctl gateway start
deploy/slack/manifest.yaml    Slack app manifest
```

## Routing stores

Five backends are supported (set via `--store` / `KLAUS_GATEWAY_STORE`):

| Store       | Value        | Persistent | Cluster-backed | Notes                                       |
|-------------|-------------|------------|----------------|---------------------------------------------|
| Memory      | `memory`    | no         | no             | Default; state lost on restart              |
| Valkey      | `valkey`    | yes        | yes            | For installations. One key per entry in Valkey (`--valkey-url`, password from `KLAUS_GATEWAY_VALKEY_PASSWORD` or `--valkey-password-file`); TTL as key expiry; every call bounded by `--valkey-timeout` |
| CRD         | `crd`       | yes        | yes            | One `ChannelRoute` CR per conversation; requires `--controller` |
| Bolt        | `bolt`      | yes        | no             | Local file; path via `--bolt-path`          |
| ConfigMap   | `configmap` | yes        | yes            | Not for installations: one `ConfigMap` holds the whole table |

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

**Compose harness** (contributor smoke test, see `deploy/README.md`):

```bash
docker compose -f deploy/docker-compose.yml up -d --build
./hack/wait-for http://127.0.0.1:8080/healthz
./hack/smoke-completion
docker compose -f deploy/docker-compose.yml down -v
```

The harness uses the `static` driver with `test-instance` mapped to the `klaus-instance`
service, a tiny Go server (`deploy/klaus-instance-stub/`) that mimics the Klaus HTTP surface.

## Conventions

- Module path: `github.com/giantswarm/klaus-gateway`
- Helm chart name: `klaus-gateway` (no `-app` suffix; this is a service repo)
- Team: `bumblebee` (annotation `application.giantswarm.io/team: bumblebee`)
- Container image: `gsoci.azurecr.io/giantswarm/klaus-gateway`
- Branch naming: `klaus/agent/<timestamp>` for agent-authored branches
- PR titles: Conventional Commits (`feat:`, `fix:`, `docs:`, `chore:`, …); the semantic PR title check is required
- Changelog: every user-visible change gets an entry under `## [Unreleased]` in `CHANGELOG.md`;
  a change an operator has to act on or decide about also gets a section in `UPGRADE.md`
- Versioning: build metadata via `-ldflags` into `pkg/project`
- Comments: explain non-obvious intent only; don't narrate code

## Gitleaks

The repo runs gitleaks on every PR. Do not include strings beginning with
`Slack bot`, `Slack app-level`, or `Slack user` anywhere in committed content, even in test
fixtures or documentation examples — they match the Slack token patterns and
will fail the scan.

## Related repos

- `giantswarm/klaus` — agent binary; `klaus-gateway` proxies traffic to its `/v1/*` and `/mcp` endpoints.
- `giantswarm/klaus-operator` — exposes MCP tools (`create_instance`, `list_instances`, …) used by the cluster lifecycle driver.
- `giantswarm/klausctl` — local lifecycle equivalent; `klaus-gateway` calls it for `--driver=klausctl` mode.
- `giantswarm/kagent-upstream` — the kagent API v2 controller `pkg/a2a` talks to; `hack/kagent-proto/` is copied from it.
- `giantswarm/muster` — the authorization server behind Slack on-behalf-of account linking (`pkg/auth/musterlink`).
- `giantswarm/agent-platform` — the meta chart that deploys `klaus-gateway` as a component and renders its routes.
- `agentgateway/agentgateway` (Linux Foundation) — the data plane; consumed via OCI image and `gateway.networking.k8s.io` CRDs.

## Documentation

Everything is in this repo:

- `docs/deployment.md` — Helm chart, agentgateway wiring, channel configuration
- `docs/channels-slack.md` and `docs/slack-hitl-surface.md` — the Slack adapter and every interactive prompt it posts
- `docs/channels-web.md`, `docs/channels-cli.md`, `docs/api.md` — the other adapters and the HTTP surface
- `docs/kagent-a2a.md` — A2A v1 over gRPC, the AgentTemplate roster, one AgentInstance per thread, HITL and stop
- `docs/development.md` — build, test, compose harness, adding an adapter
- `UPGRADE.md` — what an operator has to do or decide between releases; `CHANGELOG.md` lists every change

## Build / test

```bash
go build ./...
make test                    # what CI runs: go test ./... (race detector when cgo is available)
make lint                    # golangci-lint with gosec + goconst
./hack/helm-template-tests   # chart render check with agentgateway off and on; CI runs the chart on kind (tests/)
CGO_ENABLED=0 go build -o klaus-gateway-linux-amd64 . && docker build -t klaus-gateway:dev .   # the image copies the prebuilt binary
```

## CI

CircleCI through the `architect` orb. `.circleci/workflows.yml` is generated by devctl and rewritten
by align-files; repo-specific jobs go into `.circleci/custom.yml`, changes to the generated jobs into
devctl. On a branch: `go-build` (`make test`), `build-image` (hadolint + multi-arch build, nothing
pushed), `build-chart` and `execute-chart-tests` (the chart on a kind cluster through app-test-suite,
`tests/`). On a `v*` tag: `push-to-registries-release` (multi-arch image to gsoci and gsociprivate;
`sync-china-registry` mirrors to Aliyun without gating the chart) and `push-chart-release` (chart to
the giantswarm catalog). Tags are cut by the Auto Release GitHub workflow on every merge to `main`;
images and charts are never published by hand. Required checks on `main`: semantic PR title,
pre-commit, values schema, `go-build`, `build-image`, `build-chart`, `execute-chart-tests`.
