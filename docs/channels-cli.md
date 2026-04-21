# CLI channel adapter

The CLI adapter exposes a REST surface for remote `klausctl` sessions. It is disabled by
default and must be enabled explicitly.

## Enabling

```bash
./bin/klaus-gateway --cli-enabled
```

Or via environment variable:

```bash
KLAUS_GATEWAY_CLI_ENABLED=true ./bin/klaus-gateway
```

In Helm:

```yaml
cli:
  enabled: true
```

## Endpoints

| Method | Path                             | Description                              |
|--------|----------------------------------|------------------------------------------|
| `POST` | `/cli/v1/{instance}/run`         | Stream a completion as SSE deltas        |
| `POST` | `/cli/v1/{instance}/messages`    | Fetch message history for a session      |
| `GET`  | `/cli/v1/healthz`                | Liveness check; 200 once adapter started |

The `{instance}` path segment is used as the `channelID` in the routing key.

## POST /cli/v1/{instance}/run

Sends a user message and streams the response as Server-Sent Events.

### Request

```http
POST /cli/v1/my-instance/run HTTP/1.1
Content-Type: application/json
Authorization: Bearer <token>   (optional)

{
  "text":      "Summarise the current PR",
  "sessionId": "session-abc123",
  "userId":    "alice"
}
```

`text` and `sessionId` are required. `userId` is optional; if omitted, identity falls back
to the `Authorization` Bearer value, or `"anonymous"` if neither is provided.

The `Authorization` header value (without the `Bearer ` prefix) is passed as the `subject`
field for downstream auth passthrough.

Maximum request body: 4 MiB.

### Response

`Content-Type: text/event-stream`. Each delta is one SSE event:

```
data: {"content":"The PR adds"}

data: {"content":" streaming support."}

event: done
data: {}

```

The response also includes `X-Klaus-Instance: <name>`.

On error:

```
event: error
data: "upstream timed out"

```

### Routing key

```
channel   = "cli"
channelID = {instance} path segment
userID    = userId body field (or subject, or "anonymous")
threadID  = sessionId body field
```

If no route entry exists and `--auto-create` is enabled, the gateway creates a new Klaus
instance via the lifecycle driver. If auto-create is disabled, the request returns HTTP 404.

## POST /cli/v1/{instance}/messages

Fetches stored conversation history for a session.

### Request

```http
POST /cli/v1/my-instance/messages HTTP/1.1
Content-Type: application/json

{
  "sessionId": "session-abc123",
  "userId":    "alice"
}
```

`sessionId` is required. `userId` is optional.

### Response

```json
{
  "messages": [
    {"role": "user",      "content": "Summarise the current PR"},
    {"role": "assistant", "content": "The PR adds streaming support."}
  ]
}
```

## GET /cli/v1/healthz

Returns `200 OK` with body `ok` once the adapter has been started.

## Error responses

Non-streaming errors are returned as JSON:

```json
{
  "error": {
    "message": "text and sessionId are required",
    "type":    "Bad Request"
  }
}
```

| Status | Meaning                                                  |
|--------|----------------------------------------------------------|
| 400    | Missing required fields or malformed JSON                |
| 404    | No route entry and auto-create is disabled               |
| 413    | Request body exceeds 4 MiB                               |
| 502    | Upstream (agentgateway / Klaus instance) error           |
| 503    | Adapter not started                                      |

## Exposing through agentgateway

To route CLI traffic through the agentgateway data plane, enable the CLI route:

```yaml
agentgateway:
  enabled: true
  routes:
    cli:
      enabled: true
      prefixes:
      - /cli/v1/
```
