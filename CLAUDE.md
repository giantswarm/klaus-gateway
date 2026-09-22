# CLAUDE.md -- klaus-gateway

Project context for AI coding agents working in this repo.

## What this is

`klaus-gateway` is the **Slack** front door of the agent platform. It is **not** the LLM/MCP
data plane — that is [agentgateway](https://github.com/agentgateway/agentgateway). Keep these
roles separate when reading or writing code:

- **agentgateway** — proxies A2A, MCP and the OpenAI surface; enforces JWT/Cedar policy.
- **klaus-gateway** — runs the Slack adapter, keys every thread by
  `(channel, channelID, threadID)`, binds it to one agent and one kagent AgentInstance, and
  runs the turn as A2A v1 over gRPC through agentgateway.

Slack is the only channel. There is no web or CLI adapter, no OpenAI-compatible `/v1` front
door, no Klaus instance path and no lifecycle driver: they were removed in the Slack-only
release (issue #319).

## Stack

- Go 1.26
- HTTP: `net/http` + `chi` router (`github.com/go-chi/chi/v5`)
- Logging: `log/slog`
- OTel: `go.opentelemetry.io/otel` (OTLP gRPC)
- CI: giantswarm `architect` orb. Multi-arch (amd64+arm64) Docker images.

## Package layout

```
main.go                 entrypoint; wires the store, the kagent client, the Slack adapter, the server
pkg/a2a/                kagent API v2 client: A2A v1 over gRPC turns, AgentTemplate roster, AgentInstance per thread, HITL payloads
pkg/kagent/gen/         generated kagent.api.v1alpha1 gRPC stubs (make generate-kagent; pin in its README)
pkg/channels/           ChannelAdapter interface + Gateway facade (SendCompletion only)
pkg/channels/slack/     Slack channel adapter (/channels/slack/*); Events API + Socket Mode
pkg/routing/store/      Store interface + three backends (memory, valkey, bolt); thread state + team reviews
pkg/auth/musterlink/    Slack OBO: muster account linking + the link Store (memory, bolt file, Kubernetes Secret)
pkg/auth/satoken/       TokenReview verifier of the team-review endpoint
pkg/reviews/            the team-review endpoint (POST /reviews, /notices)
pkg/muster/             muster tool client an approved review calls through
pkg/server/             http.Server wiring, middleware, admin mux
pkg/observability/      OTel traces + Prometheus metrics
pkg/project/            build identifiers: ldflags target for version, git SHA and build timestamp; version falls back to the Go build info
internal/config/        env-var + flag config (KLAUS_GATEWAY_* prefix)
internal/version/       re-exports pkg/project for the rest of the code
helm/klaus-gateway/     Helm chart
hack/kagent-proto/      kagent protos copied from giantswarm/kagent-upstream; input of make generate-kagent
tests/                  chart tests CI runs on a kind cluster (app-test-suite): tests/ats/, tests/test-values.yaml
deploy/slack/manifest.yaml    Slack app manifest
```

## Routing stores

Three backends are supported (set via `--store` / `KLAUS_GATEWAY_STORE`):

| Store       | Value        | Persistent | Cluster-backed | Notes                                       |
|-------------|-------------|------------|----------------|---------------------------------------------|
| Memory      | `memory`    | no         | no             | Default; state lost on restart              |
| Valkey      | `valkey`    | yes        | yes            | For installations. One key per entry in Valkey (`--valkey-url`, password from `KLAUS_GATEWAY_VALKEY_PASSWORD` or `--valkey-password-file`); TTL as key expiry; every call bounded by `--valkey-timeout` |
| Bolt        | `bolt`      | yes        | no             | Local file; path via `--bolt-path`          |

The store key is `<channel>|<channelID>|<threadID>` (three parts; the user slot went with the
per-user web and CLI routes). A Slack thread's agent, initiator, grants, AgentInstance binding
and in-flight task are one row in the store, sharing one sliding lifetime (`routing.threadTTL`, default 90 days). Every writer
of that row (a channel's grant, the facade's task record, the binding) goes through
`Store.Update`, which serialises a read-modify-write per key inside the process.

## Local testing

```bash
make test                    # go test ./...
helm lint helm/klaus-gateway && helm template t helm/klaus-gateway
```

There is no compose harness and no `klausctl gateway start` path any more. The end-to-end check
is the chart smoke test CI runs on a kind cluster through app-test-suite (`tests/`).

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

- `giantswarm/kagent-upstream` — the kagent API v2 controller `pkg/a2a` talks to; `hack/kagent-proto/` is copied from it.
- `giantswarm/muster` — the authorization server behind Slack on-behalf-of account linking (`pkg/auth/musterlink`).
- `giantswarm/agent-platform` — the meta chart that deploys `klaus-gateway` as a component and renders its routes.
- `agentgateway/agentgateway` (Linux Foundation) — the data plane the kagent controller is reached through.

## Documentation

Everything is in this repo:

- `docs/deployment.md` — Helm chart, agentgateway wiring, channel configuration
- `docs/channels-slack.md` and `docs/slack-hitl-surface.md` — the Slack adapter and every interactive prompt it posts
- `docs/api.md` — the team-review endpoint and the admin surface
- `docs/kagent-a2a.md` — A2A v1 over gRPC, the AgentTemplate roster, one AgentInstance per thread, HITL and stop
- `docs/development.md` — build, test, the HTTP surface, adding an adapter
- `UPGRADE.md` — what an operator has to do or decide between releases; `CHANGELOG.md` lists every change

## Build / test

```bash
go build ./...
make test                    # what CI runs: go test ./... (race detector when cgo is available)
make lint                    # golangci-lint with gosec + goconst
helm lint helm/klaus-gateway && helm template t helm/klaus-gateway   # CI also runs the chart on kind (tests/)
CGO_ENABLED=0 go build -o klaus-gateway-linux-amd64 . && docker build -t klaus-gateway:dev .   # the image copies the prebuilt binary
```

## CI

CircleCI through the `architect` orb. `.circleci/workflows.yml` is generated by devctl and rewritten
by align-files; changes to the generated jobs go into devctl. On a branch: `go-build` (`make test`), `push-to-registries` (hadolint + amd64 build, pushed
to gsoci as a dev image `<next version>-dev.<branch>.<utc date>.<utc time>.h<sha>`; `branchPublish` in
giantswarm/github), `build-chart`, `execute-chart-tests` (the chart on a kind cluster through
app-test-suite, `tests/`, installed with the branch's own dev image) and `push-chart` (the dev chart
to the test catalog). A PR can be tried on an installation by pinning its dev tag. On a `v*` tag:
`push-to-registries-release` (multi-arch image to gsoci and gsociprivate; `sync-china-registry`
mirrors to Aliyun without gating the chart) and `push-chart-release` (chart to the giantswarm
catalog). Tags are cut by the Auto Release GitHub workflow on every merge to `main`; release images
and charts are never published by hand. Required checks on `main`: semantic PR title, pre-commit,
values schema, `go-build`, `build-chart`, `execute-chart-tests`.
