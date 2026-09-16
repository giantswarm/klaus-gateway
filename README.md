# klaus-gateway

[![CircleCI](https://dl.circleci.com/status-badge/img/gh/giantswarm/klaus-gateway/tree/main.svg?style=svg)](https://dl.circleci.com/status-badge/redirect/gh/giantswarm/klaus-gateway/tree/main)
[![License](https://img.shields.io/badge/License-Apache%202.0-blue.svg)](LICENSE)

`klaus-gateway` is the channel and routing front door for [Klaus](https://github.com/giantswarm/klaus) AI agent instances. It receives messages from human-facing channels — Slack, web browsers, and CLI sessions — maps each conversation to the right Klaus instance, creates instances on demand via [klausctl](https://github.com/giantswarm/klausctl) or [Klaus Operator](https://github.com/giantswarm/klaus-operator), and forwards LLM/MCP traffic through [agentgateway](https://github.com/agentgateway/agentgateway) as the data plane.

## Channels

| Channel | Path prefix             | Description                                    |
|---------|-------------------------|------------------------------------------------|
| Web     | `/web/*`                | Bytes-in / SSE-out adapter for web UIs         |
| Slack   | `/channels/slack/*`     | Events API webhook or Socket Mode              |
| CLI     | `/cli/v1/*`             | Remote sessions for `klausctl --remote` users  |

IDE and OpenAI/MCP-native clients bypass `klaus-gateway` and connect to `agentgateway` directly.

## Architecture

```
External consumers                  Gateway layer                        Platform
+------------------+               +---------------------------+         +-----------------+
| IDE / OpenAI SDK | -------+-----> agentgateway (data plane)  | ------> Klaus instances  |
+------------------+        |      | JWT/Cedar authn, routing  |         +-----------------+
                            |      +---------------------------+
| Slack / web      |        |      +---------------------------+         +-----------------+
| klausctl --remote| -----> +----> | klaus-gateway             | ------> Klaus Operator   |
+------------------+               | channel adapters          |         | (MCP lifecycle) |
                                   | routing table             |         +-----------------+
                                   | lifecycle drivers         |
                                   +---------------------------+
```

The chart and the agentgateway wiring are described in [docs/deployment.md](docs/deployment.md), the agent carrier under every channel in [docs/kagent-a2a.md](docs/kagent-a2a.md).

## Documentation

- [Development guide](docs/development.md) — build, test, compose harness, adding adapters
- [Deployment guide](docs/deployment.md) — Helm chart, agentgateway wiring, channel configuration
- [API reference](docs/api.md) — HTTP surface reference for all adapters
- [kagent integration](docs/kagent-a2a.md) — A2A v1 over gRPC, the AgentTemplate roster, one AgentInstance per thread, HITL and stop
- Channel guides: [Web](docs/channels-web.md) · [Slack](docs/channels-slack.md) ([interactive surface](docs/slack-hitl-surface.md)) · [CLI](docs/channels-cli.md)
- [Upgrade notes](UPGRADE.md) — what an operator has to do or decide between releases

## Quick start

```bash
# Build and test
go build ./...
make test

# Compose smoke harness: gateway + agentgateway + a Klaus stub, end to end
docker compose -f deploy/docker-compose.yml up -d --build
./hack/wait-for http://127.0.0.1:8080/healthz
./hack/smoke-completion
docker compose -f deploy/docker-compose.yml down -v

# Container image (copies the prebuilt binary)
CGO_ENABLED=0 go build -o klaus-gateway-linux-amd64 . && docker build -t klaus-gateway:dev .
```

For day-to-day development the preferred path is `klausctl gateway start`, which spins up
`klaus-gateway`, `agentgateway`, and a Klaus instance with your LLM API key. See
[docs/development.md](docs/development.md).

## Layout

```
main.go             # binary entrypoint
pkg/                # channel adapters, routing, lifecycle, server, upstream, kagent (a2a) client, OBO link store
internal/           # config, version
helm/klaus-gateway/ # Helm chart
deploy/             # docker-compose smoke harness + agentgateway config
hack/               # kagent proto sync, chart render check, smoke-harness scripts
tests/              # chart tests CI runs on kind (app-test-suite)
docs/               # development, deployment, API, and channel guides
```

## License

Apache 2.0 — see [LICENSE](LICENSE).
