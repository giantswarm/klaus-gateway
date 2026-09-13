# Web channel adapter

The web channel adapter is the HTTP surface browser-based UIs and headless drivers (the lab's
proofs) call into. It is mounted at `/web/*`; the binary enables it by default, the chart
behind `web.enabled`.

## Endpoints

| Method | Path                   | Description                                                   |
|--------|------------------------|---------------------------------------------------------------|
| `POST` | `/web/messages`        | Send a user message (or a HITL decision); receive deltas as SSE |
| `GET`  | `/web/messages`        | Fetch conversation history                                    |
| `GET`  | `/web/agents`          | List the agents a message may name                            |
| `GET`  | `/web/healthz`         | Liveness check; 200 once the adapter is started               |

The caller's `Authorization: Bearer <token>` is forwarded as the identity of the turn: on the
kagent path it is the person the agent acts as, and a request without one is refused.

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

All four fields `channelId`, `userId`, `threadId`, and `text` are required (a HITL decision may
omit `text`, see below). `subject`, `replyTo` and `agentRef` are optional; `agentRef` names the
agent of a new conversation (default: the gateway's `a2a.defaultAgent`). `attachments` is an
array of base64-encoded file objects:

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

### Prompts and decisions (human-in-the-loop)

When the agent pauses on a tool approval or a question, the stream ends with a `prompt` event
instead of `done`:

```
event: prompt
data: {"taskId":"0192…","text":"Delete the pod?","prompt":{"toolName":"kubectl_delete","hint":"Delete the pod?","tools":[{"id":"approval-1","name":"kubectl_delete","args":{"pod":"web-1"}}]}}

```

An `ask_user` prompt carries `questions` (`question`, `choices`, `multiple`) instead of `tools`.
The client answers with a new `POST /web/messages` on the same thread that names the paused
task and the decision; the response streams the resumed turn:

```json
{"channelId":"web-1","userId":"alice","threadId":"thread-7","text":"approve",
 "taskId":"0192…","decision":{"type":"approve"}}
```

`decision.type` is `approve` or `reject` (`rejectionReason` optional); an `ask_user` answer
carries `askUserAnswers`, one list of selected labels per question in order:
`{"type":"approve","askUserAnswers":[["Health check"]]}`. `text` is optional on a decision and
kept as its readable label. A decision without `taskId` is a 400.

Closing the stream mid-turn stops the turn: the gateway cancels the running task at the agent
controller.

### Routing

On the kagent path (`a2a.enabled`) the thread `(channel="web", channelID, threadID, agentRef)`
is bound to one AgentInstance created on its first turn; see [kagent-a2a.md](kagent-a2a.md).
Otherwise the gateway resolves `(channel="web", channelID, userID, threadID)` to a Klaus
instance using the routing table. If no entry exists and `--auto-create` is enabled, a new
instance is created via the lifecycle driver. If no entry exists and auto-create is disabled,
the request returns HTTP 404.

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

## GET /web/agents

Lists the agents a message's `agentRef` may name, read from the kagent controller as the
caller (the bearer token is forwarded). Only agents that can start a conversation are listed.

```json
{
  "agents": [
    {"name": "sre-agent", "namespace": "kagent", "displayName": "SRE Agent",
     "iconUrl": "https://…/sre-agent.png", "description": "Investigates infrastructure issues"}
  ]
}
```

Returns `404` on a gateway without the kagent client configured and `502` when the controller
cannot be reached (or refuses the caller).

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
