# Developing on klaus-gateway

## Toolchain

- Go 1.26
- `helm` for the chart checks
- Optional: `golangci-lint`, `gofumpt`, `pre-commit`

## Build and test

```bash
go build ./...
make test        # what CI runs: go test ./... (race detector when cgo is available)
make lint        # golangci-lint with gosec + goconst
helm lint helm/klaus-gateway
helm template t helm/klaus-gateway
pre-commit run --all-files   # includes the values-schema and helm-docs regeneration checks
```

CI additionally installs the chart on a kind cluster through app-test-suite (`tests/`).

## HTTP surface

```
# Slack channel adapter (enabled by --slack-enabled)
POST /channels/slack/events        Events API webhook endpoint
POST /channels/slack/interactions  block actions, view submissions, shortcuts
POST /channels/slack/commands      slash commands

# Slack OBO account linking (enabled by --obo-enabled)
GET  /auth/slack/link
GET  /auth/slack/callback
GET  /auth/slack/client.json       the CIMD document the OAuth client_id points at

# Team reviews (enabled by --reviews-enabled)
POST /reviews
POST /reviews/{id}/results
POST /notices

# Admin (default :8081)
GET  /healthz
GET  /readyz
GET  /metrics
```

See [docs/api.md](api.md) for the team-review and admin surfaces, and
[docs/channels-slack.md](channels-slack.md) for the Slack one.

## Project layout

```
main.go                 entrypoint; wires the store, the kagent client, the Slack adapter, the server
pkg/channels/           ChannelAdapter interface + Gateway facade
pkg/channels/slack/     Slack channel adapter (/channels/slack/*)
pkg/routing/store/      Store interface + memory / bolt / valkey backends (thread state, team reviews)
pkg/auth/musterlink/    Slack OBO: muster account linking + the link Store (memory, bolt file, Kubernetes Secret)
pkg/auth/satoken/       TokenReview verifier of the team-review endpoint
pkg/reviews/            the team-review endpoint
pkg/muster/             muster tool client used by an approved review
pkg/server/             http.Server wiring, middleware, admin mux
pkg/a2a/                kagent API v2 client (A2A v1 gRPC, AgentTemplates, AgentInstances, HITL)
pkg/kagent/gen/         generated kagent.api.v1alpha1 stubs (`make generate-kagent`)
pkg/observability/      OTel traces + Prometheus metrics
internal/config/        env-var + flag config (KLAUS_GATEWAY_* prefix)
internal/version/       ldflags-injected version metadata
```

## Adding a new channel adapter

Slack is the only channel today; the seam is still there.

1. Create `pkg/channels/<name>/` and implement `channels.ChannelAdapter`:
   - `Name() string` — stable channel identifier used as the store key's `Channel` field.
   - `Start(ctx, gw channels.Gateway) error` — wire in the `channels.Gateway` facade.
   - `Stop(ctx) error` — clean up background goroutines.
   - `Mount(r chi.Router)` — register HTTP routes.
2. Normalise inbound events into `channels.InboundMessage{Channel: ChannelName, AgentRef: ..., ...}`.
3. Call `gw.SendCompletion(ctx, msg)` and render the deltas.
4. Wire the adapter in `main.go` behind a config flag (follow the `cfg.Slack.Enabled` pattern),
   and set `InboundMessage.BearerToken` to the caller's token: the kagent client refuses a call
   without one (`pkga2a.ErrNoIdentity`).
5. Add a `KLAUS_GATEWAY_<NAME>_ENABLED` env var in `internal/config/config.go`.
6. Add channel-specific docs in `docs/channels-<name>.md`.
