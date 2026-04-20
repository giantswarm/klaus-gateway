# deploy/

This directory holds the **CI / contributor smoke harness** for klaus-gateway. It is **not** the developer path and **not** the user path:

- **Developer path:** `klausctl gateway start` spins up a local klaus-gateway + agentgateway + one klaus instance with full LLM access.
- **User path:** the Helm chart in `../helm/klaus-gateway/` running on a management cluster, fronted by an ingress.

The compose stack here exists so every PR gets a cheap, reproducible end-to-end check that a POST against the OpenAI-compat front door reaches a klaus instance (a stub in CI) and streams SSE back. `make e2e-local` drives it.

## Layout

```
deploy/
  docker-compose.yml            # three services: klaus-gateway, agentgateway, klaus-instance-stub
  agentgateway/standalone.yaml  # shared with klausctl gateway start; DO NOT fork
  klaus-instance-stub/          # tiny Go HTTP server mimicking /v1 and /mcp
```

## Run it

```bash
make e2e-local
```

Behind the scenes:

```bash
docker compose -f deploy/docker-compose.yml up -d --build
./hack/wait-for http://127.0.0.1:8080/healthz
./hack/smoke-completion
docker compose -f deploy/docker-compose.yml down -v
```

The smoke script POSTs `/v1/test-instance/chat/completions` and asserts a
delta with a `content` field streams back, then checks the web adapter at
`/web/messages`.

## Why a stub instead of the real klaus image?

`ghcr.io/giantswarm/klaus:latest` needs an LLM API key to produce output, which CI cannot safely hand out. The stub in `klaus-instance-stub/` mimics just enough of the klaus HTTP surface (`/v1/chat/completions`, `/mcp`) for the gateway to be exercised end-to-end without external dependencies. It is not a product surface.

## Why a single agentgateway config file?

`agentgateway/standalone.yaml` is consumed verbatim by both:

- the compose harness here (mounted into the agentgateway container), and
- `klausctl gateway start --with-agentgateway` locally.

Keeping one source of truth means the developer path and CI cannot drift on routing shape. Cluster-side routing (HTTPRoute / AgentgatewayBackend) lives in the Helm chart and is not generated from this file.
