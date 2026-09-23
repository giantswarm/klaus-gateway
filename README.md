# klaus-gateway

[![CircleCI](https://dl.circleci.com/status-badge/img/gh/giantswarm/klaus-gateway/tree/main.svg?style=svg)](https://dl.circleci.com/status-badge/redirect/gh/giantswarm/klaus-gateway/tree/main)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)

`klaus-gateway` is the Slack front door of the Giant Swarm agent platform. It receives Slack
messages, binds every thread to one agent and one kagent AgentInstance, and runs the turn as
A2A v1 over gRPC against the kagent controller, reached through
[agentgateway](https://github.com/agentgateway/agentgateway) as the data plane. Every call is
made as the person behind the turn: the Dex id_token of their Slack account link.

## Channels

| Channel | Path prefix         | Description                       |
|---------|---------------------|-----------------------------------|
| Slack   | `/channels/slack/*` | Events API webhook or Socket Mode |

Slack is the only channel. IDE and OpenAI/MCP-native clients connect to `agentgateway`
directly, not through this gateway.

## Architecture

```
Slack                     Gateway layer                    Platform
+---------+              +---------------------------+     +---------------------------+
| Slack   | -----------> | klaus-gateway             | --> | agentgateway (data plane) |
| Events  |              | Slack adapter             |     +---------------------------+
| Socket  |              | thread state (routing     |                  |
+---------+              |   store: memory/valkey/   |                  v
                         |   bolt)                   |     +---------------------------+
                         | muster account linking    |     | kagent controller         |
                         | team reviews              |     | (AgentTemplates,          |
                         +---------------------------+     |  AgentInstances, A2A v1)  |
                                                           +---------------------------+
```

The chart and the agentgateway wiring are described in [docs/deployment.md](docs/deployment.md),
the agent carrier under the channel in [docs/kagent-a2a.md](docs/kagent-a2a.md).

## Documentation

- [Development guide](docs/development.md) — build, test, the chart smoke test
- [Deployment guide](docs/deployment.md) — Helm chart, agentgateway wiring, channel configuration
- [API reference](docs/api.md) — the HTTP surface: Slack, muster linking, team reviews, admin
- [kagent integration](docs/kagent-a2a.md) — A2A v1 over gRPC, the AgentTemplate roster, one AgentInstance per thread, HITL and stop
- Channel guide: [Slack](docs/channels-slack.md) ([interactive surface](docs/slack-hitl-surface.md))
- [Upgrade notes](UPGRADE.md) — what an operator has to do or decide between releases

## Quick start

```bash
# Build and test
go build ./...
make test

# Container image (copies the prebuilt binary)
CGO_ENABLED=0 go build -o klaus-gateway-linux-amd64 . && docker build -t klaus-gateway:dev .
```

See [docs/development.md](docs/development.md).

## Layout

```
main.go             # binary entrypoint
pkg/                # Slack adapter, thread-state store, server, kagent (a2a) client, OBO link store, team reviews
internal/           # config, version
helm/klaus-gateway/ # Helm chart
deploy/slack/       # Slack app manifest
hack/               # kagent proto sync
tests/              # chart tests CI runs on kind (app-test-suite)
docs/               # development, deployment, API, and channel guides
```

## License

Apache 2.0 — see [LICENSE](LICENSE).
