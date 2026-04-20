# CLAUDE.md -- klaus-gateway

Project context for AI coding agents working in this repo.

## What this is

`klaus-gateway` is the channel/routing front door for [klaus](https://github.com/giantswarm/klaus) instances. It is **not** the LLM/MCP data plane -- that is [agentgateway](https://github.com/agentgateway/agentgateway). Keep these roles separate when reading or writing code:

- **agentgateway** -- proxies `/v1/*`, `/mcp`, A2A; speaks OpenAI / MCP / A2A natively; enforces JWT/Cedar policy.
- **klaus-gateway** -- exposes channel adapters (Slack, web, CLI), maps `(channel, channel-id, user, thread)` to an instance, calls `klaus-operator` (or `klausctl` locally) to create instances on demand, and forwards LLM traffic through agentgateway.

## Stack

- Go 1.26 (matches `giantswarm/lab` webapp).
- HTTP: `net/http` + `chi` router (will be added in Phase 1).
- Logging: `log/slog`.
- OTel: `go.opentelemetry.io/otel` (OTLP gRPC).
- CI: giantswarm `architect` orb. Multi-arch (amd64+arm64) Docker images.

## Layout (target, end of Phase 1)

```
cmd/klaus-gateway/main.go           # primary binary
pkg/server/                          # HTTP server, mux, middleware
pkg/api/                             # /v1/{instance}/chat/completions front door
pkg/channels/{web,slack,cli}/        # channel adapters
pkg/routing/{router,store/}          # routing table (memory / bolt / configmap / crd)
pkg/lifecycle/{klausctl,operator}/   # instance lifecycle drivers
pkg/instance/                        # HTTP client to klaus instances + SSE proxy
pkg/upstream/agentgateway.go         # agentgateway upstream wiring
pkg/auth/                            # identity mapping, downstream OAuth
pkg/observability/                   # OTel + Prometheus
internal/config/                     # env + flags + file
internal/version/                    # ldflags-injected version
helm/klaus-gateway/                  # Helm chart
deploy/docker-compose.yml            # local end-to-end harness (Phase 0b)
deploy/agentgateway/standalone.yaml  # local agentgateway config
deploy/agentgateway/kubernetes.yaml  # AgentgatewayPolicy / Backend examples
deploy/slack/manifest.yaml           # Slack app manifest (Phase 3)
docs/development.md
docs/deployment.md
```

Today only the bootstrap is in place: `main.go`, `pkg/project/`, `Dockerfile`, `helm/klaus-gateway/`, `.circleci/config.yml`.

## Conventions

- Module path: `github.com/giantswarm/klaus-gateway`.
- Helm chart name: `klaus-gateway` (no `-app` suffix; this is a service repo, not an app wrapper).
- Team: `bumblebee` (annotation `application.giantswarm.io/team: bumblebee`).
- Container image: `gsoci.azurecr.io/giantswarm/klaus-gateway`.
- Versioning: build metadata via `-ldflags` into `pkg/project`.
- Comments: explain non-obvious intent only; don't narrate code.

## Reference repos in the lab

- `giantswarm/klaus` -- agent binary; klaus-gateway proxies traffic to its `/v1/*` and `/mcp` endpoints.
- `giantswarm/klaus-operator` -- exposes MCP tools (`create_instance`, `list_instances`, ...) used by the cluster lifecycle driver.
- `giantswarm/klausctl` -- local lifecycle equivalent; klaus-gateway calls it for `--store=bolt --driver=klausctl` mode.
- `giantswarm/lab` -- webapp that will switch its `chatproxy.go` to talk to klaus-gateway in Phase 2a.
- `giantswarm/muster` -- MCP federation; not a dependency, runs alongside.
- `agentgateway/agentgateway` (Linux Foundation, upstream) -- the data plane; consumed via OCI image and `gateway.networking.k8s.io` CRDs.

The full plan and architecture doc live in [`teemow/klaus-lab`](https://github.com/teemow/klaus-lab) under `architecture/klaus-gateway.md` and `decisions/2026-04-20-1100-adr-agentgateway-as-data-plane.md`.

## Build / test

```bash
go build ./...
go test ./...
docker build -t klaus-gateway:dev .
```

## CI

CircleCI via the `architect` orb: `go-build`, `push-to-registries-multiarch`, `push-to-app-catalog`. Branch builds run amd64 only; tag builds (`v*`) run full multi-arch.
