# Developing on klaus-gateway

## Toolchain

- Go 1.26
- `docker` + `docker compose` (for the smoke harness)
- Optional: `golangci-lint`, `gofumpt`

## Build and test

```bash
go build ./...
go test -race ./...
```

## Running it against a real klaus instance

For day-to-day hacking, the **developer path** is `klausctl gateway start`, which spins up klaus-gateway, agentgateway, and one klaus instance locally with your LLM API key plumbed through. Details live in the klausctl repo.

## Running the compose smoke harness

The compose stack in `deploy/docker-compose.yml` is a **CI/contributor smoke harness**, not the developer path. It exists so every PR gets a cheap end-to-end check.

```bash
make e2e-local
```

This brings up three services:

- `klaus-gateway` (built from this repo)
- `agentgateway` (`ghcr.io/agentgateway/agentgateway:v1.1.0`, config from `deploy/agentgateway/standalone.yaml`)
- `klaus-instance-stub` (tiny Go server that mimics the klaus HTTP surface)

POST a chat completion against the gateway:

```bash
curl -N -H 'Content-Type: application/json' \
  -d '{"stream":true,"messages":[{"role":"user","content":"ping"}]}' \
  http://127.0.0.1:8080/v1/test-instance/chat/completions
```

You should see OpenAI-style SSE chunks with `{"content": "..."}` deltas and a terminating `[DONE]` line. That proves the wiring: request -> klaus-gateway -> agentgateway -> klaus-instance and SSE back.

Tear it down:

```bash
make e2e-local-down
```

## Front door surface

```
POST /v1/{instance}/chat/completions   # OpenAI-compat, SSE passthrough
POST /v1/{instance}/chat/messages      # MCP `messages` tool as JSON
POST /web/messages                     # web channel adapter, SSE out
GET  /web/messages?channelId=&userId=&threadId=
GET  /web/healthz
```

The `/v1/{instance}/...` shape is deliberate: OpenAI SDKs work by only setting `baseURL` to `http://klaus-gateway/v1/<instance>`. See `docs/api.md`.

## Project layout

```
cmd/klaus-gateway/    entrypoint
pkg/api/              OpenAI-compat front door (/v1/{instance}/...)
pkg/channels/         ChannelAdapter + Gateway facade
pkg/channels/web/     web channel adapter (/web/*)
pkg/instance/         HTTP client for klaus instances + SSE helpers
pkg/lifecycle/        lifecycle.Manager interface + drivers (klausctl, operator, static)
pkg/routing/          routing table and pluggable stores
pkg/server/           http.Server wiring, middleware, admin mux
pkg/upstream/         agentgateway upstream rewriter
internal/config/      env + flag config
```
