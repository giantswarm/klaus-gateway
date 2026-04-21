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
make helm-test   # helm lint + render assertions
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
make e2e-local        # bring up, run smoke, tear down
make e2e-local-up     # bring up in the background only
make e2e-local-down   # tear down and remove volumes
```

This brings up three services:

| Service               | Image / build                                                  | Role                                |
|-----------------------|----------------------------------------------------------------|-------------------------------------|
| `klaus-gateway`       | built from this repo                                           | gateway under test                  |
| `agentgateway`        | `ghcr.io/agentgateway/agentgateway:v1.1.0`                    | LLM/MCP data plane                  |
| `klaus-instance-stub` | built from `deploy/klaus-instance-stub/`                       | tiny HTTP stub mimicking Klaus      |

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
cmd/klaus-gateway/      entrypoint; wires stores, lifecycle drivers, adapters, server
pkg/api/                OpenAI-compat front door (/v1/{instance}/...)
pkg/api/v1alpha1/       ChannelRoute CRD types (routing.giantswarm.io/v1alpha1)
pkg/channels/           ChannelAdapter interface + Gateway facade
pkg/channels/web/       web channel adapter (/web/*)
pkg/channels/slack/     Slack channel adapter (/channels/slack/*)
pkg/channels/cli/       CLI channel adapter (/cli/v1/*)
pkg/instance/           HTTP client for Klaus instances + SSE helpers
pkg/lifecycle/          lifecycle.Manager interface + drivers (klausctl, operator, static)
pkg/routing/            routing table + pluggable store backends
pkg/routing/store/      Store interface + memory / bolt / configmap / crd backends
pkg/server/             http.Server wiring, middleware, admin mux
pkg/upstream/           agentgateway upstream URL rewriter
pkg/observability/      OTel traces + Prometheus metrics
internal/config/        env-var + flag config (KLAUS_GATEWAY_* prefix)
internal/controller/    ChannelRoute controller-runtime reconciler
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
4. Wire the adapter in `cmd/klaus-gateway/main.go` behind a config flag (follow the
   `cfg.Slack.Enabled` / `cfg.CLI.Enabled` pattern).
5. Add a `KLAUS_GATEWAY_<NAME>_ENABLED` env var in `internal/config/config.go`.
6. Add channel-specific docs in `docs/channels-<name>.md`.
