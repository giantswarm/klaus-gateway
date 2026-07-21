# How klaus-gateway renders agent output

This is a map of how an agent turn becomes channel UI. klaus-gateway receives a neutral
stream of deltas from a Klaus instance (over A2A) and each channel adapter decides how to
present them. The Slack adapter is the richest surface and the focus here; web and CLI
consume the same neutral model with simpler rendering.

All anchors are `file:line` against this repo.

## 1. The neutral model

The gateway never hands a channel adapter kagent/ADK-shaped data. Everything is normalised
into two package-level types:

- `InboundMessage` (`pkg/channels/channel.go:49-78`): what an adapter sends up per user turn
  (text, optional `Decision` for a HITL answer, `TaskID` to resume a paused task,
  `BearerToken` for on-behalf-of egress).
- `OutboundDelta` (`pkg/channels/channel.go:123-142`): one chunk streamed back. `DeltaKind`
  (`channel.go:84-88`) classifies it:
  - `DeltaText`: assistant text.
  - `DeltaToolActivity`: a tool call or result, carried in `Tool` (`ToolActivity`,
    `channel.go:109-118`).
  - `DeltaPrompt`: the agent is paused waiting for the user. `Prompt` (`*HitlPrompt`) is set
    when the pause carried a structured approval/ask_user request; `TaskID` identifies the
    paused A2A task to resume.
  - `Usage` (`*TurnUsage`, `channel.go:90-99`) rides any delta reporting token counts.

The kagent/ADK-to-neutral translation lives in `pkg/channels/hitl_parse.go`: `parseHitlPrompt`
(`:66`) reads the `adk_request_confirmation` DataPart into a `HitlPrompt`, and
`buildInboundParts` (`:39`) turns a `HitlDecision` back into the A2A DataPart kagent expects.
HITL types are in `pkg/channels/hitl.go`.

## 2. Text streaming

`batchedWriter` (`pkg/channels/slack/stream.go:40`) accumulates `DeltaText` content and
flushes on a 250ms ticker (`run`, `:101`), staying under Slack's ~4 updates/sec/channel. Each
flush (`:403`) renders the buffer as a Block Kit `markdown` block via `chat.update` on the
reply message. A reply over Slack's 12 000-char block limit (`slackMarkdownBlockMax`, `:28`)
splits (`splitMarkdown`) and rolls the overflow into stable follow-up in-thread messages.

The head message may be posted lazily on the first flush (reactions mode) or seeded as a
placeholder up front (text mode); see progress, below.

## 3. Tool activity

`renderToolActivity` (`stream.go:175`) posts a compact record of each tool call (and, at
`detailsFull`, its result) as a fenced code block so Slack collapses long payloads. Verbosity
is per-thread, set by `/details on|off|full` (`commands.go`, stored in `transparency.go:36-60`);
the default is `detailsOn` (calls only). A per-turn cap (`maxToolMessages`, `stream.go:167`)
posts one truncation note and then goes quiet so a tool-heavy turn does not flood the thread.
Posts are handed to an async poster so a slow Slack API never stalls delta draining.

## 4. HITL (approval and ask_user)

When the stream ends on a `DeltaPrompt`, the writer hands the prompt to `postHitlPrompt`
(`pkg/channels/slack/hitl.go:84`), which branches:

- **Generic tool approval** (`ToolName` != `ask_user`): `postApprovalPrompt`
  (`stream.go:661`) posts Approve / Deny / Chat buttons. Chat holds the task and swaps in a
  reply hint so the user can ask a follow-up before deciding.
- **ask_user, single question**: `chooseChoiceRender` (`hitl.go:68`) picks a layout:

  | choices | multiple | max label runes | render | commit |
  |---|---|---|---|---|
  | 0 | any | . | numbered text + free-text reply | reply |
  | 1-10 | false | <=75 | `radio_buttons`, one per line (`postChoiceWidgetPrompt`, `stream.go:742`) | Submit |
  | 1-10 | true | <=75 | `checkboxes`, one per line (`postChoiceWidgetPrompt`) | Submit |
  | 1-10 | false | >75 | section per choice + accessory button (`postChoiceSectionPrompt`, `stream.go:785`) | click |
  | 1-10 | true | >75 | section per choice + accessory checkbox (`postChoiceSectionPrompt`) | Submit |
  | >10 | any | any | numbered text + free-text reply (`renderAskUserText`, `hitl.go:113`) | reply |

- **ask_user, multiple questions**: if every question is a widget (1-10 choices, each label
  <=75 runes) and there are at most `maxFormQuestions`, the prompt renders as a single form
  (`postChoiceFormPrompt`, `stream.go`) — one `radio_buttons`/`checkboxes` group per question,
  each block_id-tagged with its question index (`hitl_q_<qi>`), committed by one Submit
  (`formRenderable`, `hitl.go`). Otherwise (any free-text / over-long / over-count question, or
  too many questions) it falls back to numbered text + free-text reply, one answer line per
  question.

A choice is a bare `string` end to end: kagent's `ask_user` tool schema is
`Choices []string` with no per-choice description or header, so there is nothing richer to
render (`parseAskUserQuestions`, `hitl_parse.go:117`). The user can always answer any prompt
by replying in-thread (`decisionFromText`, `hitl.go:137`).

### Verified Block Kit limits (docs.slack.dev)

| Limit | Value |
|---|---|
| `button.text` / option-object `text` (select, radio, checkbox) | 75 |
| option-object `description` | 75 |
| `radio_buttons` / `checkboxes` options | 10 |
| `section.text` | 3000 |
| actions-block elements | 25 |
| blocks per message | 50 |

No selectable widget renders a label over 75 runes without truncation, which is why the
long-label case falls back to a `section` per choice (3000-char text) carrying only the choice
index on its control. Radio/checkbox change dispatches a `block_actions` payload; the widget
paths ignore the change and act only on Submit.

### Answer round-trip

```
click → POST /channels/slack/interactions → routeInteraction (interactions.go:140)
  classifyAction → hitlApprove | hitlDeny | hitlChat | hitlChoice | hitlSubmit
  hitlChoice  : decodeChoiceValue(value) → {thread, index}         (section accessory button)
  hitlSubmit  : selectedChoiceIndices(payload.State) → []int       (single-question, read state.values)
              + selectedChoicesByQuestion(payload.State) → map[qi][]int  (multi-question form, per hitl_q_<qi>)
  handleDecision (interactions.go:309): access check, thread lock, human-token mint
    form: reject an incomplete multi-question Submit (re-store task + nudge) before deciding
    buildButtonDecision (interactions.go:440): indices → labels → HitlDecision{approve, AskUserAnswers}
      single question → one answer slot; multi-question form → one slot per question, in order
  chat.update rewrites the prompt to the chosen answer
  gw.SendCompletion resumes the paused task; buildInboundParts (hitl_parse.go:39) emits the DataPart
```

A Submit with nothing selected nudges the user and leaves the task pending (`interactions.go`,
`choiceSelectNudge`). Only a permitted user (the thread initiator or a granted collaborator)
may decide; an onlooker click is refused ephemerally (`handleDecision`).

## 5. Connector login (agent needs a backend the user hasn't connected)

When a `core_auth_login` tool result carries a backend login URL, `maybeConnectorPrompt`
(`stream.go:220`) parses it out of the free-text result (`parseAuthChallenge`) and
`postConnectorPrompt` (`stream.go:872`) posts an ephemeral "Connect <server>" URL button plus
"Not now". It is throttled per (user, backend) by a cooldown so an ignored prompt does not
repeat every message. The Connect button opens the backend's consent flow in the browser; the
gateway does nothing on the click (`routeInteraction`, `connectorConnect`).

## 6. OBO sign-in (act-as-user account linking)

`postSignInPrompt` (`stream.go:831`) posts an ephemeral "Sign in to Giant Swarm" URL button.
`/login` and `/logout` (`commands.go:200`) drive linking explicitly. On click the gateway
stores the interaction's `response_url` (`oboSignIn`, `interactions.go`); once the browser link
completes, `notifyLinked` replaces the ephemeral prompt in place. A turn that needs a user
token but has none degrades: the tool call fails at the backend rather than running as the
gateway identity.

## 7. Access consent (a newcomer wants to instruct the agent in someone's thread)

`postAccessConsentPrompt` (`stream.go:929`) posts an ephemeral Yes/No to the thread initiator.
`handleAccessDecision` (`interactions.go:268`) grants (additively) and replays the newcomer's
held messages on Yes, discards on No; only the initiator may decide.

## 8. Progress and interruption

`startProgress` (`progress.go:71`) picks reactions mode (swap a working emoji on the trigger
message, then done/failed) or text mode (a "thinking..." placeholder replaced by the answer),
per `ProgressMode` (auto falls back to text when the bot lacks `reactions:write`). `/stop`
cancels an in-flight turn, or rejects a paused task so the dangling tool call is resolved.

## 9. Usage

`/usage` (`commands.go`) reports last-turn and session token totals via `usageReport`
(`transparency.go:111`), summed from the `Usage` on each delta (kagent reports usage per LLM
call, so the writer accumulates across the turn). An optional model label is resolved from the
agent's CRD and cached.

## 10. Other channels

The web and CLI adapters (`pkg/channels/web`, `pkg/channels/cli`) consume the same
`OutboundDelta` stream. Interactive HITL widgets (buttons, radio, checkbox) are Slack-specific;
those channels present prompts as text and take a typed answer, which `decisionFromText`
(`hitl.go:137`) maps to the same structured `HitlDecision`.
