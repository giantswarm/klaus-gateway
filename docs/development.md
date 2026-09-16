# Developing on klaus-gateway

## Toolchain

- Go 1.26
- `docker` + `docker compose` (for the smoke harness)
- Optional: `golangci-lint`, `gofumpt`

## Build and test

```bash
go build ./...
go test -race ./...
make lint        # golangci-lint with gosec + goconst
./hack/helm-template-tests   # chart render check with agentgateway off and on
```

## Developer path

For day-to-day hacking the preferred path is `klausctl gateway start`, which spins up
`klaus-gateway`, `agentgateway`, and one Klaus instance locally with your LLM API key
plumbed through. Details live in the [klausctl](https://github.com/giantswarm/klausctl) repo.

## Compose smoke harness

The compose stack in `deploy/docker-compose.yml` is the **CI / contributor smoke harness** —
not the developer path. It exists so every PR gets a cheap end-to-end check without
requiring `klausctl` or a real LLM key.

```bash
docker compose -f deploy/docker-compose.yml up -d --build   # bring up
./hack/wait-for http://127.0.0.1:8080/healthz              # wait for the gateway
./hack/smoke-completion                                     # run the smoke test
docker compose -f deploy/docker-compose.yml down -v         # tear down and remove volumes
```

This brings up three services:

| Service               | Image / build                                                  | Role                                |
|-----------------------|----------------------------------------------------------------|-------------------------------------|
| `klaus-gateway`       | built from this repo                                           | gateway under test                  |
| `agentgateway`        | `ghcr.io/agentgateway/agentgateway` (tag pinned in the compose file) | LLM/MCP data plane                  |
| `klaus-instance`      | built from `deploy/klaus-instance-stub/`                       | tiny HTTP stub mimicking Klaus      |

The gateway is configured with the `static` driver (`KLAUS_GATEWAY_DRIVER=static`), so it
maps `test-instance` to the stub without needing `klausctl` or `Klaus Operator`.

POST a chat completion to verify the wiring:

```bash
curl -N -H 'Content-Type: application/json' \
  -d '{"stream":true,"messages":[{"role":"user","content":"ping"}]}' \
  http://127.0.0.1:8080/v1/test-instance/chat/completions
```

You should see OpenAI-style SSE chunks with `{"content":"..."}` deltas ending with `[DONE]`.

## HTTP surface

```
# OpenAI-compatible front door
POST /v1/{instance}/chat/completions   OpenAI-compat, SSE passthrough
POST /v1/{instance}/chat/messages      MCP messages tool as JSON

# Web channel adapter
POST /web/messages                     send user message, receive deltas as SSE
GET  /web/messages?channelId=&userId=&threadId=
GET  /web/healthz

# Slack channel adapter (enabled by --slack-enabled)
POST /channels/slack/events            Events API webhook endpoint

# CLI channel adapter (enabled by --cli-enabled)
POST /cli/v1/{instance}/run            stream completion as SSE deltas
POST /cli/v1/{instance}/messages       fetch message history for a session
GET  /cli/v1/healthz

# Admin (default :8081)
GET  /healthz
GET  /readyz
GET  /metrics
```

The `/v1/{instance}/...` shape lets any OpenAI SDK work by setting
`baseURL = "http://klaus-gateway/v1/<instance>"`. See [docs/api.md](api.md).

## Project layout

```
main.go                 entrypoint; wires stores, lifecycle drivers, adapters, server
pkg/api/                OpenAI-compat front door (/v1/{instance}/...)
pkg/channels/           ChannelAdapter interface + Gateway facade
pkg/channels/web/       web channel adapter (/web/*)
pkg/channels/slack/     Slack channel adapter (/channels/slack/*)
pkg/channels/cli/       CLI channel adapter (/cli/v1/*)
pkg/instance/           HTTP client for Klaus instances + SSE helpers
pkg/lifecycle/          lifecycle.Manager interface + drivers (klausctl, operator, static)
pkg/routing/            routing table + pluggable store backends
pkg/routing/store/      Store interface + memory / bolt / valkey backends
pkg/auth/musterlink/    Slack OBO: muster account linking + the link Store (memory, bolt file, Kubernetes Secret)
pkg/server/             http.Server wiring, middleware, admin mux
pkg/upstream/           agentgateway upstream URL rewriter
pkg/a2a/                kagent API v2 client (A2A v1 gRPC, AgentTemplates, AgentInstances, HITL)
pkg/kagent/gen/         generated kagent.api.v1alpha1 stubs (`make generate-kagent`)
pkg/observability/      OTel traces + Prometheus metrics
internal/config/        env-var + flag config (KLAUS_GATEWAY_* prefix)
internal/version/       ldflags-injected version metadata
```

## Adding a new channel adapter

1. Create `pkg/channels/<name>/` and implement `channels.ChannelAdapter`:
   - `Name() string` — stable channel identifier used as the routing key `Channel` field.
   - `Start(ctx, gw channels.Gateway) error` — wire in the `channels.Gateway` facade.
   - `Stop(ctx) error` — clean up background goroutines.
   - `Mount(r chi.Router)` — register HTTP routes.
2. Normalise inbound events into `channels.InboundMessage{Channel: ChannelName, ...}`.
3. Call `gw.Resolve(ctx, msg)` to get an `instance.Ref`, then `gw.SendCompletion(ctx, ref, msg)`.
4. Wire the adapter in `main.go` behind a config flag (follow the
   `cfg.Slack.Enabled` / `cfg.CLI.Enabled` pattern).
5. Add a `KLAUS_GATEWAY_<NAME>_ENABLED` env var in `internal/config/config.go`.
6. Add channel-specific docs in `docs/channels-<name>.md`.
