# klaus-gateway

Channel and routing gateway in front of [klaus](https://github.com/giantswarm/klaus) instances.

`klaus-gateway` is the human-facing channel layer (Slack, web, CLI, IDE) that routes inbound messages to the right klaus instance and proxies LLM/MCP/A2A traffic through [agentgateway](https://github.com/agentgateway/agentgateway) as the data plane.

## Architecture

```
External consumers                Gateway layer                       Platform
+-----------------+               +-------------------+               +-----------------+
| IDE / OpenAI    | ----------->  | agentgateway      |  --->  klaus-instance pods
| Slack / web     | --> klaus-gateway -> agentgateway |
| klausctl --remote |             | (channel adapters,|
+-----------------+               |  routing,         |               +-----------------+
                                  |  lifecycle)       |  --->  klaus-operator (MCP)
                                  +-------------------+
```

- IDE and other OpenAI/MCP-native clients hit `agentgateway` directly.
- Channel-bound traffic (Slack, web, CLI sessions) goes through `klaus-gateway`, which resolves channel identity to an instance, creates instances on demand, and forwards the request to `agentgateway`.

See the [architecture document](https://github.com/teemow/klaus-lab/blob/main/architecture/klaus-gateway.md) and the [agentgateway-as-data-plane ADR](https://github.com/teemow/klaus-lab/blob/main/decisions/2026-04-20-1100-adr-agentgateway-as-data-plane.md) in the lab notebook for the full design.

## Status

Phase 1 is live. The binary ships:

- OpenAI-compat front door at `/v1/{instance}/chat/completions` and `/v1/{instance}/chat/messages` (path-scoped so OpenAI SDKs work with just a `baseURL` change).
- Web channel adapter at `/web/*` -- bytes-in / SSE-out, consumed by the lab webapp.
- Routing table with memory / bolt / configmap stores.
- Lifecycle drivers: `klausctl`, `operator`, and a `static` driver for compose / single-instance deployments.
- Upstream wiring to agentgateway (or direct-to-instance).
- OTel traces + Prometheus `/metrics`.
- Compose smoke harness in `deploy/` driven by `make e2e-local`.

Slack adapter, CLI adapter, cluster Helm for agentgateway CRDs, and the ChannelRoute CRD land in follow-up PRs.

## Layout

```
cmd/                # entrypoints (added in Phase 1)
pkg/                # exported packages (server, channels, routing, lifecycle, ...)
internal/           # internal helpers (config, build metadata)
helm/klaus-gateway/ # Helm chart
deploy/             # docker-compose harness, agentgateway examples (Phase 0b)
docs/               # development.md, deployment.md
```

## Build

```bash
go build ./...
docker build -t klaus-gateway:dev .
```

## License

Apache 2.0 -- see [LICENSE](LICENSE).
