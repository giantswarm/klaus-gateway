# Web channel adapter

The web channel adapter is the HTTP surface the lab webapp — and any other browser-based UI —
calls into. It is mounted at `/web/*` and is always enabled (no configuration flag required).

## Endpoints

| Method | Path                   | Description                                     |
|--------|------------------------|-------------------------------------------------|
| `POST` | `/web/messages`        | Send a user message; receive LLM deltas as SSE  |
| `GET`  | `/web/messages`        | Fetch conversation history                      |
| `GET`  | `/web/healthz`         | Liveness check; 200 once the adapter is started |

## POST /web/messages

Sends a user message and streams the response as Server-Sent Events.

### Request

```http
POST /web/messages HTTP/1.1
Content-Type: application/json

{
  "channelId": "web-session-42",
  "userId":    "alice",
  "threadId":  "thread-7",
  "text":      "What is the capital of France?",
  "subject":   "oauth-sub-optional",
  "replyTo":   "",
  "attachments": []
}
```

All four fields `channelId`, `userId`, `threadId`, and `text` are required. `subject` and
`replyTo` are optional. `attachments` is an array of base64-encoded file objects:

```json
{
  "filename":    "notes.txt",
  "contentType": "text/plain",
  "bytes":       "<base64>"
}
```

Maximum request body: 4 MiB (attachments included).

### Response

`Content-Type: text/event-stream`. Each delta is one SSE event:

```
data: {"content":"Par"}

data: {"content":"is"}

event: done
data: {}

```

The response also includes `X-Klaus-Instance: <name>` so the client knows which Klaus
instance handled the turn.

On error the stream emits an `event: error` line:

```
event: error
data: "upstream timed out"

```

### Routing

The gateway resolves `(channel="web", channelID, userID, threadID)` to a Klaus instance
using the routing table. If no entry exists and `--auto-create` is enabled, a new instance
is created via the lifecycle driver. If no entry exists and auto-create is disabled, the
request returns HTTP 404.

## GET /web/messages

Fetches stored conversation history for a thread.

### Request

```
GET /web/messages?channelId=web-session-42&userId=alice&threadId=thread-7
```

All three query parameters are required.

### Response

```json
{
  "messages": [
    {"role": "user",      "content": "What is the capital of France?", "sent_at": "2026-04-20T12:00:00Z"},
    {"role": "assistant", "content": "Paris.",                          "sent_at": "2026-04-20T12:00:01Z"}
  ]
}
```

## GET /web/healthz

Returns `200 OK` with body `ok` once the adapter has been started. Returns `503` if the
adapter has not yet started or has been stopped.

## Error responses

Non-streaming errors are returned as JSON:

```json
{
  "error": {
    "message": "channelId, userId, threadId, text are all required",
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
