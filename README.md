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

Full design: [architecture doc](https://github.com/teemow/klaus-lab/blob/main/architecture/klaus-gateway.md) · [agentgateway ADR](https://github.com/teemow/klaus-lab/blob/main/decisions/2026-04-20-1100-adr-agentgateway-as-data-plane.md)

## Documentation

- [Development guide](docs/development.md) — build, test, compose harness, adding adapters
- [Deployment guide](docs/deployment.md) — Helm chart, agentgateway wiring, channel configuration
- [API reference](docs/api.md) — HTTP surface reference for all adapters
- Channel guides: [Web](docs/channels-web.md) · [Slack](docs/channels-slack.md) · [CLI](docs/channels-cli.md)

## Quick start

```bash
# Run the compose smoke harness (builds + end-to-end test)
make e2e-local

# Build binary
go build ./...

# Build container image
docker build -t klaus-gateway:dev .
```

For day-to-day development the preferred path is `klausctl gateway start`, which spins up
`klaus-gateway`, `agentgateway`, and a Klaus instance with your LLM API key. See
[docs/development.md](docs/development.md).

## Layout

```
cmd/                # binary entrypoint
pkg/                # channel adapters, routing, lifecycle, server, upstream
internal/           # config, controller, version
helm/klaus-gateway/ # Helm chart
deploy/             # docker-compose smoke harness + agentgateway config
docs/               # development, deployment, API, and channel guides
```

## License

Apache 2.0 — see [LICENSE](LICENSE).
