# HITL & interactive surface (Slack)

Every interactive prompt klaus-gateway can post to Slack (as the **Swarmgeist** app), when it
appears, and how the user answers it. The gateway receives a neutral stream of deltas from the
thread's kagent Session (over A2A); when a turn pauses for the user, the Slack adapter renders one of
the prompts below. Prompt building lives in `pkg/channels/slack/` (`stream.go`, `hitl.go`);
click handling lives in `interactions.go`.

Each section carries a Block Kit JSON you can paste into the
[Block Kit Builder](https://app.slack.com/block-kit-builder) to see the exact widget, plus a
slot for a real screenshot. Question and choice text is agent-authored and shown here as
example values; the `action_id` / `block_id` / `style` / `value` shapes match what the adapter
emits (JSON has no comments, so anything you would swap is left as an obvious example).

## Which prompt renders

A single ask_user question picks its layout by choice count, select mode, and label length:

| questions | choices | select | max label runes | render | commit |
|---|---|---|---|---|---|
| 1 | 0 | — | — | numbered text + free-text reply | reply |
| 1 | 1–10 | single | ≤75 | `radio_buttons`, one per line | Submit |
| 1 | 1–10 | multi | ≤75 | `checkboxes`, one per line | Submit |
| 1 | 1–10 | single | >75 | section per choice + accessory **button** | click (immediate) |
| 1 | 1–10 | multi | >75 | section per choice + accessory **checkbox** | Submit |
| 1 | >10 | — | — | numbered text + free-text reply | reply |
| 2–20 | each 0 or 1–10 | any | each ≤75 | single **form** (a group per question with choices, a text input per question without) | one Submit |
| >20, or any question >10 / label >75 | — | — | — | numbered text + free-text reply | reply |

Generic (non-`ask_user`) tool approvals always render as the approval card, with Approve and
Deny buttons. A task paused on an MCP server's URL elicitation (a sign-in link, muster's tool
approval page, any later one) renders as the generic prompt of section 7.
Any prompt can also be answered by replying in-thread (a reply that opens with a mention of
someone other than the bot is not for the agent and answers nothing); a reply resolves the paused task the
same way a click does. Only a permitted user (the thread initiator or a granted collaborator)
may decide; an onlooker click is refused ephemerally. A thread is one shared session, so any
permitted user may answer a prompt it raised. The team review (section 10) is the exception:
it has no initiator, and any linked member of its team may decide.

## 1. Tool approval

The agent paused on a tool call that needs approval (`ToolName` is not `ask_user`). The
gateway posts an approval card:

- **Section:** `*Approval required* · <tools>`. The tools are named in plain language: muster's
  meta-tools get phrases of their own, and every other name drops its `x_`/`workflow_` namespace
  and is capitalised ("Capi list clusters"). Muster's `call_tool` is unwrapped to the tool it
  runs, and several calls decided together are listed with commas. A second line carries the
  agent's hint, escaped, but only when it adds something: the ADK runtime's default hint
  ("Please approve or reject the tool call call_tool() by responding with a FunctionResponse…")
  and the tool-name fallback of a status without text are left out.
- **Context:** `<@initiator> or the people they allowed can decide`. The call runs with the
  identity of whoever decides.
- **Buttons:** Approve (primary) runs the call, Deny (danger) rejects it. There is no "Ask a
  question" button. A typed reply that is not "approve" or "deny" is sent as a rejection with
  the text as its reason, but the Go ADK runtime drops that reason, so the model sees only
  "call is rejected" (giantswarm/kagent-upstream#71). The commands are the exception:
  a reply that is only a command word (`usage`, `help`, `agents`, `login`, `logout`) runs that command
  and leaves the card open, since rejecting a call with the reason "logout" is what nobody
  meant. `mute` is the one command that decides the card: it mutes the thread and rejects the
  card the way a deny word does. Beside a **question** — the
  `ask_user` card, or one without a card, where a typed reply is the answer — those words stay
  answers, and the command waits until the question is answered (`channels-slack.md`, "Every
  command is a plain word"); `mute` there refuses instead, privately, and is not the answer.
- **After a decision:** the buttons go and a context line names who decided, for a click and
  for a typed reply alike: `Approved by <@U123> · <time>` for "approve", `Denied by <@U123> ·
  <time>` for "deny" and for any other reply. A card posted for a status without a structured
  prompt sends a typed reply to the agent as text, so it reads `Answered by <@U123> · <time>`.

`value` is the JSON `{"t":"<thread>","id":"<task>"}`; the task binds the buttons to the
prompt they render, so a click on a superseded prompt is refused instead of answering a newer
one. The card states no expiry. The pending task is kept for 24 hours in the thread's row in
the routing store, so on a persistent store a click after a restart still answers it.

```json
{
  "blocks": [
    {
      "type": "section",
      "text": { "type": "mrkdwn", "text": "*Approval required* · Capi list clusters" }
    },
    {
      "type": "context",
      "elements": [ { "type": "mrkdwn", "text": "<@U123> or the people they allowed can decide" } ]
    },
    {
      "type": "actions",
      "elements": [
        { "type": "button", "text": { "type": "plain_text", "text": "Approve" }, "style": "primary", "action_id": "hitl_approve", "value": "{\"t\":\"THREAD_TS\",\"id\":\"TASK_ID\"}" },
        { "type": "button", "text": { "type": "plain_text", "text": "Deny" }, "style": "danger", "action_id": "hitl_deny", "value": "{\"t\":\"THREAD_TS\",\"id\":\"TASK_ID\"}" }
      ]
    }
  ]
}
```

A click updates the card in place. The section stays, and one context line replaces the
context and the buttons: `Approved by <@U123> · <!date^…^{time}|16:30 UTC>` or `Denied by …`
(Slack shows the time in each reader's time zone). A click whose task is gone (answered,
dropped, or lost in a restart) or superseded by a newer prompt also keeps the section, and the
note (`_Already answered._` or the superseded notice) replaces the buttons. A Chat button
(`hitl_chat`) exists only on cards an earlier gateway version posted; a click on it is refused
the same way.

## 2. ask_user — single question, radio buttons (1–10 choices, single-select)

One `radio_buttons` group plus a Submit button. Each option's `value` is its choice index;
the Submit reads the selection out of `state.values`.

```json
{
  "blocks": [
    { "type": "section", "text": { "type": "mrkdwn", "text": "*Which region should I deploy to?*" } },
    {
      "type": "actions",
      "block_id": "hitl_group_block",
      "elements": [
        {
          "type": "radio_buttons",
          "action_id": "hitl_group",
          "options": [
            { "text": { "type": "plain_text", "text": "eu-central-1" }, "value": "0" },
            { "text": { "type": "plain_text", "text": "us-east-1" }, "value": "1" },
            { "text": { "type": "plain_text", "text": "ap-southeast-2" }, "value": "2" }
          ]
        }
      ]
    },
    {
      "type": "actions",
      "elements": [
        { "type": "button", "text": { "type": "plain_text", "text": "Submit" }, "style": "primary", "action_id": "hitl_submit", "value": "{\"t\":\"THREAD_TS\",\"id\":\"TASK_ID\"}" }
      ]
    }
  ]
}
```

<img width="718" height="270" alt="image" src="https://github.com/user-attachments/assets/8352d69a-b5ce-4e47-9795-5c66f798f2f3" />

## 3. ask_user — single question, checkboxes (1–10 choices, multi-select)

Same as above with a `checkboxes` group instead of `radio_buttons`, so more than one choice
can be selected before Submit.

```json
{
  "blocks": [
    { "type": "section", "text": { "type": "mrkdwn", "text": "*Which add-ons should I enable?*" } },
    {
      "type": "actions",
      "block_id": "hitl_group_block",
      "elements": [
        {
          "type": "checkboxes",
          "action_id": "hitl_group",
          "options": [
            { "text": { "type": "plain_text", "text": "Logging" }, "value": "0" },
            { "text": { "type": "plain_text", "text": "Monitoring" }, "value": "1" },
            { "text": { "type": "plain_text", "text": "Ingress" }, "value": "2" }
          ]
        }
      ]
    },
    {
      "type": "actions",
      "elements": [
        { "type": "button", "text": { "type": "plain_text", "text": "Submit" }, "style": "primary", "action_id": "hitl_submit", "value": "{\"t\":\"THREAD_TS\",\"id\":\"TASK_ID\"}" }
      ]
    }
  ]
}
```

<img width="718" height="270" alt="image" src="https://github.com/user-attachments/assets/7655168d-ce47-48ec-86f0-9a4be4a7f952" />

## 4. ask_user — single question, long labels, single-select (section + button)

When a choice label exceeds 75 runes it can't fit a widget option, so each choice becomes a
section carrying the full label (up to 3000 chars) with an accessory **button**. One choice
per row is unambiguous, so a click commits immediately — no Submit. Each button's `action_id`
is `hitl_choice_<index>` and its `value` is the JSON `{"t":"<thread>","c":<index>,"id":"<task>"}`.

```json
{
  "blocks": [
    { "type": "section", "text": { "type": "mrkdwn", "text": "*Which migration plan do you want?*" } },
    {
      "type": "section",
      "text": { "type": "mrkdwn", "text": "Blue/green: stand up a parallel cluster, cut over DNS once healthy, keep the old one for fast rollback." },
      "accessory": { "type": "button", "text": { "type": "plain_text", "text": "Select" }, "action_id": "hitl_choice_0", "value": "{\"t\":\"THREAD_TS\",\"c\":0,\"id\":\"TASK_ID\"}" }
    },
    {
      "type": "section",
      "text": { "type": "mrkdwn", "text": "In-place: drain and upgrade nodes one at a time; lower cost, longer window, no parallel capacity." },
      "accessory": { "type": "button", "text": { "type": "plain_text", "text": "Select" }, "action_id": "hitl_choice_1", "value": "{\"t\":\"THREAD_TS\",\"c\":1,\"id\":\"TASK_ID\"}" }
    }
  ]
}
```

<img width="714" height="232" alt="image" src="https://github.com/user-attachments/assets/d89cf37e-37c6-4e73-b5e8-000820102d19" />

## 5. ask_user — single question, long labels, multi-select (section + checkbox)

Multi-select long-label variant: each choice is a section with an accessory single-option
**checkbox** (its `block_id` is `hitl_group_block_<index>` for a stable per-row id), committed
by a Submit that gathers the selected rows.

```json
{
  "blocks": [
    { "type": "section", "text": { "type": "mrkdwn", "text": "*Which checks should block the release?*" } },
    {
      "type": "section",
      "block_id": "hitl_group_block_0",
      "text": { "type": "mrkdwn", "text": "End-to-end suite on a fresh workload cluster (adds ~20 min but catches upgrade regressions)." },
      "accessory": { "type": "checkboxes", "action_id": "hitl_group", "options": [ { "text": { "type": "plain_text", "text": "Select" }, "value": "0" } ] }
    },
    {
      "type": "section",
      "block_id": "hitl_group_block_1",
      "text": { "type": "mrkdwn", "text": "Conformance + CVE scan on the built images before promotion to the catalog." },
      "accessory": { "type": "checkboxes", "action_id": "hitl_group", "options": [ { "text": { "type": "plain_text", "text": "Select" }, "value": "1" } ] }
    },
    {
      "type": "actions",
      "elements": [
        { "type": "button", "text": { "type": "plain_text", "text": "Submit" }, "style": "primary", "action_id": "hitl_submit", "value": "{\"t\":\"THREAD_TS\",\"id\":\"TASK_ID\"}" }
      ]
    }
  ]
}
```

<img width="720" height="336" alt="image" src="https://github.com/user-attachments/assets/fb858be8-0f7a-46df-b90b-6e3ecbf4d406" />


## 6. ask_user — multiple questions, single form

When every question is widget-renderable (1–10 choices, each label ≤75 runes) or has no
choices, and there are 2–20 of them, the whole prompt renders as one form: a section per
question, then a radio/checkbox group for a question with choices or an optional multi-line
text input (`input` block, `plain_text_input` with `action_id` `hitl_text`) for a question
without, committed by a single Submit. Each group's and input's `block_id` is
`hitl_q_<question index>` so the handler maps each answer back to its question. A muted line
under the Submit says a reply in the thread answers the form too, one line per question. The
Submit resumes only once every question with choices is answered; an incomplete Submit nudges
("Choose an answer for every question with options") and leaves the form pending. A text input
left empty answers its question with an empty slot, but a form whose questions all lack choices
needs at least one typed answer ("Type at least one answer"). The `*bold*` wrapping each
question is added by the gateway.

```json
{
  "blocks": [
    { "type": "section", "text": { "type": "mrkdwn", "text": "*Which database?*" } },
    {
      "type": "actions",
      "block_id": "hitl_q_0",
      "elements": [
        {
          "type": "radio_buttons",
          "action_id": "hitl_group",
          "options": [
            { "text": { "type": "plain_text", "text": "PostgreSQL" }, "value": "0" },
            { "text": { "type": "plain_text", "text": "MySQL" }, "value": "1" }
          ]
        }
      ]
    },
    { "type": "section", "text": { "type": "mrkdwn", "text": "*Which features?*" } },
    {
      "type": "actions",
      "block_id": "hitl_q_1",
      "elements": [
        {
          "type": "checkboxes",
          "action_id": "hitl_group",
          "options": [
            { "text": { "type": "plain_text", "text": "Auth" }, "value": "0" },
            { "text": { "type": "plain_text", "text": "Logging" }, "value": "1" }
          ]
        }
      ]
    },
    { "type": "section", "text": { "type": "mrkdwn", "text": "*Anything else?*" } },
    {
      "type": "input",
      "block_id": "hitl_q_2",
      "optional": true,
      "label": { "type": "plain_text", "text": "Your answer" },
      "element": { "type": "plain_text_input", "action_id": "hitl_text", "multiline": true, "max_length": 3000 }
    },
    {
      "type": "actions",
      "elements": [
        { "type": "button", "text": { "type": "plain_text", "text": "Submit" }, "style": "primary", "action_id": "hitl_submit", "value": "{\"t\":\"THREAD_TS\",\"id\":\"TASK_ID\"}" }
      ]
    },
    { "type": "context", "elements": [ { "type": "mrkdwn", "text": "Or reply in this thread, one line per question." } ] }
  ]
}
```

<img width="714" height="359" alt="image" src="https://github.com/user-attachments/assets/21e82e7c-0a78-429c-9e93-a684f527a6a3" />

## 7. URL elicitations (sign-in, tool approval, any later one)

A sign-in an agent needs in a turn, and a tool call muster asks the person to approve (a tool set
to *Ask*), are both the runtime's URL elicitation: the task pauses at `input-required` with the
server's `url_elicitation_request`, and the gateway posts the prompt below ("A URL elicitation a
paused task waits on"). The gateway reads no link out of the agent's text or tool results, and
it does not tell a sign-in from an approval or any later elicitation: they look the same.

Every link follows MCP's URL mode elicitation. The prompt shows the whole link before the person
opens it: the domain it opens in bold ("Opens *muster.example*"), then the URL exactly as it
opens, in code so Slack neither shortens nor links it. An internationalized domain is shown in
its `xn--` punycode form with a warning that it may imitate another one; a non-ASCII domain with
no punycode form is shown as written, with the same warning. The prompt's text is `verbatim`, so
Slack links no URL in the server's message: the button is the one way to open a link. The
server's message gets the room the section has left, so the link is never cut. The link button
(`connector_connect`) is neutral, labelled *Open ↗* and the host it opens ("Open ↗
muster.example"), and its click continues nothing. A link that is not https (http only on a
loopback host), carries credentials, is longer than 2000 bytes (each `&` counted as `&amp;`) or
cannot be shown as written (spaces, control and format characters such as bidirectional
overrides, backticks, `<` and `>`) is not offered.

**Done and the generic return.** Every elicitation prompt carries *Done* (`action_id`
`elicitation_done`, `value` the prompt's completion state). Its first click by the person the
prompt is for is the `accept` of the elicitation: what the prompt holds continues, and muster
decides the outcome on the retried call (it runs the call once the sign-in completed or the
approval was granted, refuses it when it was declined or expired, or, for an approval not
decided yet, asks again: muster does not wait for an approval). A *Done* or *Not now* that
continues nothing says why instead of doing nothing: "This prompt was already answered." after
an earlier click, muster's return or its flow settled; "This prompt can no longer continue: ..."
when the process holds no state for it (below), telling the person to ask again.

A link on muster's origin (the origin of `obo.musterUrl`: scheme, host and port, a default port
written or not) gets one parameter, `redirect=<obo.callbackBaseUrl>/auth/slack/return?prompt=<key>`,
where `<key>` is a random 128-bit value the gateway mints for that one prompt; a `redirect` the
link already named is replaced, never kept. muster's
`oauth.mcpClient.postLoginRedirectAllowlist` matches the path as a prefix and leaves the query
free, and muster keeps the query when it sends the browser back: after an approval with the
`elicitationId` appended, after a sign-in with `server=<name>` (and no `elicitationId`). The
gateway's return continues the one prompt whose key the request carries, still waiting and not
past its lifetime, as its *Done* would, as the person it was posted for, and only when an
`elicitationId` the request carries is that prompt's own. So both kinds come back automatically:
approvals and sign-ins on muster's origin alike. The key is in the link of an ephemeral prompt
only its person sees, so a bare `elicitationId` (which the server that sent the elicitation
chose or knows) resumes nothing, and a prompt of another server's link (no return, no key) is
never resumed by a return. The return page only says to go back to Slack; it never says what
was decided, and an unknown, used or missing key gets the same page. muster drops a redirect its
allowlist does not admit (and logs it), so without the allowlist entry *Done* is the one way
back. Any link off muster's origin opens exactly as the server gave it. The return cannot
rewrite the ephemeral (it has no `response_url`), so the prompt keeps its buttons; a later
*Done* on it says it was already answered.

`obo.musterUrl` must be muster's public origin, the one its links are on
(`aggregator.oauth.mcpClient.publicUrl`); with an in-cluster address there no link gets the
return, and *Done* is the one way back.

**One process holds a prompt.** The completion states (what *Done* and the return continue)
live in the memory of the process that posted the prompt, for 30 minutes (muster's longest
`approval.ttl`). After a restart, or with more than one replica when the click or muster's
return reaches another replica than the one that posted the prompt, nothing continues: *Done*
says the prompt can no longer continue and to ask again, and the return page still says to
choose *Done* if the conversation does not continue. The chart runs one replica by default.

The gateway's own prompts (a Session start, a decision, a team review, a conversation reply)
also carry *Not now* (`action_id` `connector_decline`, `value` the completion state). Its first
click by the person the prompt is for declines the elicitation to muster as the person (the
`decline`: muster runs nothing; on a `-32042` elicitation nothing is sent), drops what the prompt
held and rewrites it to "Not now. What waited for it was not sent; try again when you're
ready."; the prompt's *Done* then continues nothing. A held Session start's thread line reads
"@Jose chose not now." and its DM card says the same; a decision or a team review loses the
status line naming the person and stays open. A workspace provider's Connect prompt is a
sign-in, not an elicitation: its button keeps *Sign in* (or *Connect <provider>*), it has no
*Done* or *Not now*, and its click continues nothing either.

The gateway's own prompts are laid out like a runtime's: muster's message as the heading, in
bold on one line, then the flow's line on what waits for it ("Asked by *muster* before your answer
goes through." for a decision, "... before your approval goes through." for a team review, "...
before your reply goes to *Your guide*." for a conversation reply, "... before the conversation
can start." for a Session start), the link, then the flow's tail ("Open it, then choose *Done*:
..."). The status line a decision or team review shows meanwhile reads "@Jose is finishing a step
at muster to answer as themselves." When muster still asks on the retry of *Done* (an approval
not decided yet), the person gets the prompt again, its line starting "Not done yet: *muster*
still asks", the status line reads "@Jose is not done at muster's link yet; their answer waits
for it.", and what waited stays held (a conversation reply is not dropped); the new prompt's
*Done* continues it. A retry muster refuses (a declined or expired approval) is shown as the
flow's refusal.

The prompt answers a Session start muster asked for (the person has not consented to the
server's GitHub App, or `start_session` asks for approval). The message is held. *Done* accepts
the elicitation as the person: the gateway retries the start (with muster's `requestState` and
the `accept` on 2026-07-28, the plain call on 2025-11-25); muster waits up to five minutes for a
sign-in to complete, or matches an approval, and then starts the Session, and the held message
runs as the thread's first turn (agent-manager's `start_session` is idempotent per request id,
so the replayed start finds that Session). What muster still asks for (a sign-in that did not
complete in time, an approval not decided yet) is asked for again by the replayed start, with a
fresh prompt. A retry muster refuses (the person declined the approval, it expired) is not
replayed: the line reads "@Jose is done; the conversation did not start.", the card and a note
to the person in the thread read "Your message was not sent: <muster's words>", and the held
message is dropped. In a DM the card is a message of the conversation; a later message gets a
card of its own, and the earlier one is rewritten to "Use the newer prompt below." In a channel
thread it is ephemeral, so the same card, led by "Asked in a thread in #channel." (the thread
linked), also goes to the person's DM with the app, and a context line in the thread says whom
it waits for and where the prompt is ("Waiting for @Jose at *muster*'s link (the prompt is in a
direct message from the app)"); a later message of the person's refreshes the DM card in place,
and the line stays. Once the start ran, the line reads "@Jose is done." and the card is a
confirmation: "Done. Your message is on its way to the agent." when *Done* ran the held
message, "Done." when their next turn that starts the Session found nothing to ask and the held
message stays unsent. Every surface's buttons carry the prompt's completion state in their
`value`; a *Done* on the ephemeral replaces it at once through its `response_url` (Slack marks
the click `container.is_ephemeral`) with "Done. What waited for it goes ahead.", the cards are
rewritten in place once the start settled.

```json
{
  "blocks": [
    { "type": "section", "text": { "type": "mrkdwn", "verbatim": true, "text": "*Sign in to agent-manager to continue.*\nAsked by *muster* before the conversation can start.\nOpens *muster.example*\n`https://muster.example/oauth/proxy/start?redirect=https%3A%2F%2Fgateway.example%2Fauth%2Fslack%2Freturn%3Fprompt%3D<key>&amp;state=abc`\nOpen it, then choose *Done*: your message goes to the agent." } },
    {
      "type": "actions",
      "elements": [
        { "type": "button", "text": { "type": "plain_text", "text": "Open ↗ muster.example" }, "style": "primary", "action_id": "connector_connect", "value": "<completion state>", "url": "https://muster.example/oauth/proxy/start?redirect=https%3A%2F%2Fgateway.example%2Fauth%2Fslack%2Freturn%3Fprompt%3D<key>&state=abc" },
        { "type": "button", "text": { "type": "plain_text", "text": "Done" }, "action_id": "elicitation_done", "value": "<completion state>" },
        { "type": "button", "text": { "type": "plain_text", "text": "Not now" }, "action_id": "connector_decline", "value": "<completion state>" }
      ]
    }
  ]
}
```

### A URL elicitation a paused task waits on

Every runtime the platform runs (the claude, codex and omp Harnesses, the Go ADK) relays MCP URL
elicitations: it pauses the task at `input-required` with the server's link
(`url_elicitation_request`, see `docs/kagent-a2a.md`). The server named is the MCP server that
asked, muster for a platform agent; what is asked is in its message ("Sign in to github to
continue.", "Alert Review wants to acknowledge an incident in PagerDuty."). The link is minted for
the person whose turn asked, so the prompt goes to them alone, ephemeral (`postConnectEphemeral`),
and the thread gets "Waiting for @Jose (asked by *muster*).". The prompt's heading is the server's
message in bold, on one line; then "Asked by *muster*.", the link, and "Open it, then choose
*Done* here: the agent continues." The message and the server's name are shown as written and
nothing more: control and format characters (bidirectional overrides, zero-width characters)
are dropped, and Slack's formatting characters (`*`, `~`, a backtick, an `_` not inside a word)
become full-width look-alikes, so they can neither end the bold nor format anything. A host
too long for the *Open ↗* button is cut on the left at a label ("Open ↗ …evil.example"), so the
button always shows the registrable domain; when not even that fits, the button reads *Open ↗*
and the text above shows the host. A server that sent no message is named instead ("*muster* asks
you to open a link so the agent can continue."). The buttons are *Open ↗ <host>*
(`connector_connect`), *Done* (`elicitation_done`), *Not now* (`action_id` `sign_in_decline`,
`value` the thread and the paused task) and *Authorize all* where offered. *Done*, or muster's
return for this prompt, resumes the paused task with `accept` ("Done, continue") and
the prompt reads "Done. The agent continues."; the runtime retries its call, and the outcome is
the server's (muster waits for a sign-in on a 2026-07-28 retry and matches an approval
server-side). *Not now* resumes it with `decline`, and the prompt reads "Not now. The agent
continues without it." A reply in the thread answers it too: a refusal (`no`, `cancel`, `not
now`) declines, anything else accepts. The thread line then reads "@Jose is done." or "@Jose
chose not now."; neither says what was decided. A *Done* for a task no longer paused on that
elicitation resumes nothing. When the task was answered elsewhere first (Backstage answers the
same request), *Done*, *Not now* or a reply sends nothing, and the thread reads "This was
already answered elsewhere, so this answer was not sent to the agent.". An approval or a
question answered elsewhere ends the same way.

```json
{
  "blocks": [
    { "type": "section", "text": { "type": "mrkdwn", "verbatim": true, "text": "*Alert Review wants to acknowledge an incident in PagerDuty.*\nAsked by *muster*.\nOpens *muster.example*\n`https://muster.example/approvals/0f3a9c?redirect=https%3A%2F%2Fgateway.example%2Fauth%2Fslack%2Freturn%3Fprompt%3D<key>`\nOpen it, then choose *Done* here: the agent continues." } },
    {
      "type": "actions",
      "elements": [
        { "type": "button", "text": { "type": "plain_text", "text": "Open ↗ muster.example" }, "style": "primary", "action_id": "connector_connect", "value": "<completion state>", "url": "https://muster.example/approvals/0f3a9c?redirect=https%3A%2F%2Fgateway.example%2Fauth%2Fslack%2Freturn%3Fprompt%3D<key>" },
        { "type": "button", "text": { "type": "plain_text", "text": "Done" }, "action_id": "elicitation_done", "value": "<completion state>" },
        { "type": "button", "text": { "type": "plain_text", "text": "Not now" }, "action_id": "sign_in_decline", "value": "{\"t\":\"<thread>\",\"id\":\"<task>\"}" }
      ]
    }
  ]
}
```

Where the gateway can read the person's grant status (see the authorize modal below), every
muster server's Connect prompt carries *Authorize all* after its link button (`action_id`
`authorize_all_open`). A workspace
provider's Connect prompt does not: the modal grants muster servers, not a provider account.

### Authorize your tools (modal)

A modal (`callback_id` `authorize_modal`, close button *Done*), opened by *Authorize all*, by
`/<command> connect` and from the direct message after sign-in. It reads "*1 of 2* tools are
authorized. Authorize each one once; the agent can use it for you from then on, in every
conversation.", then one section per server reached with a grant of the person's: its name and
state (*Authorized*, *Needs your authorization*, *Authorize again*, *Deactivated by its
administrator*, *Not reachable right now*, or muster's reason), and while the grant is missing
the link as a Connect prompt shows it (domain in bold, the whole URL, the punycode warning) with
an *Authorize* URL button (`action_id` `authorize_grant_open`, `value` the server). A link the
gateway would not offer in a Connect prompt gets no button, and the row says so. A context line
counts the servers that come with the sign-in; *Refresh* (`authorize_refresh`) redraws it with
new links. *Authorize* opens muster's link unchanged and continues nothing in Slack: the person
redraws the modal with *Refresh* once they are back, and a Connect prompt the modal was opened
from still waits for its own *Done*.

```json
{
  "type": "modal",
  "callback_id": "authorize_modal",
  "title": { "type": "plain_text", "text": "Authorize your tools" },
  "close": { "type": "plain_text", "text": "Done" },
  "blocks": [
    { "type": "section", "text": { "type": "mrkdwn", "text": "*1 of 2* tools are authorized. Authorize each one once; the agent can use it for you from then on, in every conversation." } },
    { "type": "divider" },
    { "type": "section", "text": { "type": "mrkdwn", "verbatim": true, "text": "*agent-manager*\n:large_orange_circle: Needs your authorization\nOpens *muster.example*\n`https://muster.example/oauth/proxy/start?state=abc`" },
      "accessory": { "type": "button", "text": { "type": "plain_text", "text": "Authorize" }, "style": "primary", "action_id": "authorize_grant_open", "value": "agent-manager", "url": "https://muster.example/oauth/proxy/start?state=abc" } },
    { "type": "section", "text": { "type": "mrkdwn", "verbatim": true, "text": "*github*\n:large_green_circle: Authorized" } },
    { "type": "context", "elements": [ { "type": "mrkdwn", "text": "3 more come with your sign-in and need nothing else." } ] },
    { "type": "actions", "elements": [ { "type": "button", "text": { "type": "plain_text", "text": "Refresh" }, "action_id": "authorize_refresh" } ] }
  ]
}
```

## 8. OBO sign-in (act-as-user account linking)

Posted when a turn needs the user's token but they haven't linked their account, and by
`login` for a linked user whose sign-in muster's token endpoint would not renew; there the
card follows the ephemeral line "Your Giant Swarm sign-in could not be refreshed, so here is
a new sign-in link." In a
channel it is ephemeral to that user, and the same card, led by "Asked in a thread in
#channel." (the thread linked), goes to the user's DM, where it waits for a Slack client that
was closed; a later message in the thread re-issues the ephemeral and refreshes the DM card in
place. A thread notice (a context line) anchors the ephemeral and
names who the thread waits for ("Waiting for @Pau and @Jose to sign in to Giant Swarm (the
sign-in link is in a direct message from the app)"), carries no link and is posted once per thread; it drops each person who signs in, and once
nobody waits it reads "@Pau and @Jose signed in to Giant Swarm". In a DM it is a threaded
message. The card's last line depends on what asked for it: "Your message runs as soon as
you sign in." for a held message, "Sign in, then click the button again." for a button
click, nothing for `login`. The button opens the linking flow. Once the link completes, a
DM prompt is rewritten in place to the signed-in confirmation ("Signed in to Giant Swarm.
The agent now acts with your permissions."). A channel prompt is replaced with the same
text through the `response_url` of its Sign in click, so its button goes; when the click
did not reach this process, or the replace fails, it is confirmed with a fresh ephemeral.
A fresh prompt that replaces an expired one starts with the context line "The earlier
sign-in link expired. Use this one." The link expires after 15 minutes; a later message
posts a fresh prompt. A DM prompt is rewritten to say its link expired; a channel prompt
cannot be rewritten, so the fresh ephemeral says the earlier link expired instead. A turn
that still has no user token is aborted rather than run as the gateway identity. In a
channel the button's `value` is `<thread>|<prompt id>`, so a click is matched to the card
it came from and a click on an older card in the thread cannot take the current card's
`response_url`; a DM prompt's button has no value.

```json
{
  "blocks": [
    { "type": "section", "text": { "type": "mrkdwn", "text": "*Sign in to Giant Swarm*\nThe agent runs its tools with your own permissions, so it needs your sign-in once. The link is valid for 15 minutes. Your message runs as soon as you sign in." } },
    {
      "type": "actions",
      "elements": [
        { "type": "button", "text": { "type": "plain_text", "text": "Sign in" }, "style": "primary", "action_id": "obo_sign_in", "url": "https://example.com/login", "value": "1712345678.000100|K3VQ7X2M4PZJ6WTBN5HRC8DY2A" }
      ]
    },
    { "type": "context", "elements": [ { "type": "mrkdwn", "text": "Signed in before? Your session may have expired." } ] }
  ]
}
```

<img width="721" height="159" alt="image" src="https://github.com/user-attachments/assets/1d629999-8f4a-41be-ba10-70d2c3a0fa57" />

## 9. Access consent (a newcomer wants to instruct the agent in someone's thread)

Ephemeral, shown only to the thread initiator. Allow grants the newcomer (additively) and
replays their held message; Decline discards it and tells the newcomer (ephemerally) that the
owner declined. The held message is kept in the thread's row, so a click after a restart on a
persistent store still replays it. A click on a prompt whose thread state the gateway no longer
holds (a restart on `routing.store: memory`, expiry) rewrites the prompt to say the approval
expired. Each button's `value` is
the JSON `{"t":"<thread>","u":"<newcomer>"}`, since one initiator can have several pending
approvals at once.

A granted collaborator's turns run in the initiator's conversation under the collaborator's
own identity: the gateway sends their own token together with the thread's Session
share, and names them to the agent as attribution. See
[Threads and conversations](channels-slack.md#threads-and-conversations).

```json
{
  "blocks": [
    { "type": "section", "text": { "type": "mrkdwn", "text": "*<@U0NEWCOMER> wants to join this thread*\nThey would see this conversation and instruct the agent in it, under their own sign-in. Allow them?" } },
    {
      "type": "actions",
      "elements": [
        { "type": "button", "text": { "type": "plain_text", "text": "Allow" }, "style": "primary", "action_id": "access_allow", "value": "{\"t\":\"THREAD_TS\",\"u\":\"U0NEWCOMER\"}" },
        { "type": "button", "text": { "type": "plain_text", "text": "Decline" }, "style": "danger", "action_id": "access_deny", "value": "{\"t\":\"THREAD_TS\",\"u\":\"U0NEWCOMER\"}" }
      ]
    }
  ]
}
```

<img width="721" height="159" alt="image" src="https://github.com/user-attachments/assets/8249895e-3da1-41e6-9e23-1d42bb5a26b2" />

## 10. Team review (a manager's ask to a team)

Posted into a team's channel — not into a thread — by a manager through
[`POST /reviews`](api.md#team-review-endpoint): the change spelled out (what, which repository,
the giving and the receiving team for a transfer; for an action the actor, the targets, the
capability and the inputs that changed), the pull requests it lands as (one link per line,
`owner/repo#n`), an *Approve* button, a *Deny* button when the manager names a deny tool, and a
URL button for anything else, labelled by what it opens (*Open PR*, *Open run*, *Open issue*,
*Open repository*, *Open link*). It has **no initiator**; the decision rule is the team's:

- **Any linked member may decide.** The clicker needs a linked identity (the OBO sign-in). An
  unlinked clicker is asked to sign in; the review stays open.
- **The actor does not approve their own action.** A review that names its actor (by the email
  of their linked identity) refuses the actor's *Approve* with a status line under the buttons —
  a second person decides. The actor's *Deny* withdraws the action.
- **Deny takes a reason.** The *Deny* click opens a modal with one required box; on submit the
  manager's deny tool is called as the member with the arguments plus `reason`. The modal claims
  nothing until it is submitted, so one left open holds the review for nobody. The denied message
  reads `*Denied* by <@U…> for team-bumblebee.`, the ask, the reason as a quote, then what the
  tool said.
- **A second channel is informed.** A review naming a `noticeChannel` posts the same text, pull
  requests and link there as a notice (section 11) before the review itself.
- **The results come back into the thread.** The manager posts the action's outcome — merged,
  rolled out, probes green or the failing probe — through `POST /reviews/{id}/results`; each is a
  reply in the review message's thread, so one thread carries the whole action.
- **The click calls the review's tool as that member.** The gateway calls the named muster tool
  (giantswarm-repo-manager's `approve_change`) with the clicking member's own token, so the
  manager acts under that person's GitHub grant and checks their membership of the named team
  there. A refusal — not a member, the author of the change themselves, or anything else the
  manager will not do — is written once under the buttons, naming the clicker and the reason, and
  the review stays open for another member; so is a manager the gateway could not reach.
- **A backend the person has not connected yet is connected from the click.** When muster answers
  the call with a sign-in (the tool's server holds no grant for the person), the clicker gets
  an ephemeral Connect prompt with muster's message and the whole link (section 7). Their
  *Done* accepts it: the gateway retries the call with the `accept`, muster waits for the
  sign-in and then runs it, so one click on the review, the sign-in and *Done* are all it takes.
  A tool approval muster asks for there is the same prompt. The
  wait is bounded below the review's claim lease, so no other click takes the review over
  meanwhile. A sign-in that did not complete in time is reported once, not looped.
- **The clicker and the team read the same line.** A status line under the buttons names the
  latest attempt that did not decide the review — who is connecting, whose approval the manager
  refused and why, whose could not be submitted. It is replaced on every attempt and gone once
  the review is approved. Nothing is repeated to the clicker privately; a channel-level ephemeral
  is reserved for what is theirs alone — a sign-in or Connect button, or a click on a review
  somebody else decided.
- **One decision closes it.** A second click is refused with who decided and how (or whose
  decision is in flight); the message is rewritten to the outcome, the decider and what the tool
  answered — a plain text as written, a JSON object by its `message` field, structured data
  without one not at all — the pull requests still listed and the link kept as small print.

The Approve and Deny buttons' `value` is the JSON `{"r":"<review id>"}`; the id is what
`POST /reviews` returned. The Deny modal (`callback_id: team_review_deny`) carries
`{"r":"<review id>","c":"<channel>"}` as `private_metadata` and reads the reason from
`state.values.team_review_deny_reason.reason`.

```json
{
  "blocks": [
    { "type": "section", "text": { "type": "mrkdwn", "text": "*Review for team-bumblebee*\n*Enable* `agent-platform` on two installations for <@U…>: …" } },
    { "type": "section", "text": { "type": "mrkdwn", "text": "• <https://github.com/giantswarm/a-configs/pull/12|giantswarm/a-configs#12>\n• <https://github.com/giantswarm/b-management-clusters/pull/7|giantswarm/b-management-clusters#7>" } },
    {
      "type": "actions",
      "elements": [
        { "type": "button", "text": { "type": "plain_text", "text": "Approve" }, "style": "primary", "action_id": "team_review_approve", "value": "{\"r\":\"REVIEW_ID\"}" },
        { "type": "button", "text": { "type": "plain_text", "text": "Deny" }, "style": "danger", "action_id": "team_review_deny", "value": "{\"r\":\"REVIEW_ID\"}" },
        { "type": "button", "text": { "type": "plain_text", "text": "Open run" }, "action_id": "team_review_open", "url": "https://github.com/giantswarm/platform-manager/actions/runs/4242", "value": "{\"r\":\"REVIEW_ID\"}" }
      ]
    }
  ]
}
```

While an attempt is pending the message carries a context block under the actions, such as
`<@U…> is finishing a step at muster to approve as themselves.`,
`<@U…> is not done at muster's link yet; their approval waits for it.` or
`<@U…>'s approval was not accepted: …`. After the approval the message reads
`*Approved* by <@U…> for team-bumblebee.`, then the ask, then the tool's answer in italics,
with `<url|Open PR>` as a context block. The review is a record in the gateway's routing store
for seven days (see [`POST /reviews`](api.md#post-reviews)): on a store that outlives the process
a gateway restart changes nothing for the team, and a click on a review the gateway no longer
holds — seven days passed, or `routing.store: memory` restarted — rewrites the message to say it
expired. The completion state behind a review's *Done* button is the process's own: after a
restart its click says the prompt can no longer continue, and the person clicks *Approve* again.

## 11. Team notice (no decision)

The variant without buttons, through [`POST /notices`](api.md#post-notices) or as a review's
`noticeChannel` copy: a completion message, or the giving team's notice of a transfer. The pull
requests, when given, are links one per line; the link, when given, is a context line labelled
by its kind.

```json
{
  "blocks": [
    { "type": "section", "text": { "type": "mrkdwn", "text": "*For team-honeybadger*\n`giantswarm/old-thing` moved to team-bumblebee." } },
    { "type": "context", "elements": [ { "type": "mrkdwn", "text": "<https://github.com/giantswarm/github/pull/4711|Open PR>" } ] }
  ]
}
```

## 12. Agent picker (starting a conversation)

The one modal the gateway opens, and the only prompt here that no agent raised: the user asked
to start a conversation. Two entry points open the same view (`callback_id: ask_agent`) — the
slash command (`/swarmgeist [question]`, whose text prefills the **Prompt** box) and the **Ask
an agent here** message shortcut (⋯ menu → Apps, `callback_id: ask_agent_here`). The view is
titled **New conversation**. It opens with a context line that says where the conversation
lands: "Starts a thread in #channel under the agent's name…" for the slash command, "Continues
this thread in #channel…" for the shortcut in a thread, "Starts this message's thread in
#channel…" for the shortcut on a top-level message without replies (Slack sends no `thread_ts`
for it), and in a DM the same without the channel and without "Anyone in the channel can read
it…", because a DM has no other readers. The select lists the live roster as the caller, the
default agent preselected and named in the select's hint ("SRE Agent is the default agent.", only
when the default is on the list). Both selects in the picker are `static_select`s, which Slack
filters as the person types, so each is one search-and-dropdown field with no round trip per
keystroke; their placeholders say so ("Type to search or pick an agent"). The submit button reads **Start thread** for
the slash command and **Start conversation** for the shortcut, whose thread exists already.
`private_metadata` carries where the picker was opened, how to answer the user privately, and —
for the shortcut — the thread the conversation starts in. In a channel, the shortcut's view
carries one more block: a checkbox, ticked, offering the thread's earlier messages to the agent.
It names no count — counting would mean reading the thread before `views.open`, and Slack
invalidates the trigger three seconds after issuing it — and the input is `optional`, so
clearing the box still submits.

```json
{
  "type": "modal",
  "callback_id": "ask_agent",
  "private_metadata": "{\"c\":\"C123\",\"u\":\"U123\",\"r\":\"https://hooks.slack.com/actions/…\",\"t\":\"1699999999.000100\"}",
  "title": { "type": "plain_text", "text": "New conversation" },
  "submit": { "type": "plain_text", "text": "Start conversation" },
  "close": { "type": "plain_text", "text": "Cancel" },
  "blocks": [
    {
      "type": "context",
      "elements": [ { "type": "mrkdwn", "text": "Continues this thread in <#C123> under the agent's name. Anyone in the channel can read it; you decide who may instruct the agent." } ]
    },
    {
      "type": "input",
      "block_id": "ask_agent_agent",
      "label": { "type": "plain_text", "text": "Agent" },
      "hint": { "type": "plain_text", "text": "SRE Agent is the default agent." },
      "element": {
        "type": "static_select",
        "action_id": "agent",
        "placeholder": { "type": "plain_text", "text": "Type to search or pick an agent" },
        "options": [ { "text": { "type": "plain_text", "text": "SRE Agent" }, "value": "kagent/sre-agent" } ],
        "initial_option": { "text": { "type": "plain_text", "text": "SRE Agent" }, "value": "kagent/sre-agent" }
      }
    },
    {
      "type": "input",
      "block_id": "ask_agent_question",
      "label": { "type": "plain_text", "text": "Prompt" },
      "element": { "type": "plain_text_input", "action_id": "question", "multiline": true, "max_length": 3000, "placeholder": { "type": "plain_text", "text": "What should the agent look into?" } }
    },
    {
      "type": "input",
      "block_id": "ask_agent_context",
      "optional": true,
      "label": { "type": "plain_text", "text": "Thread context" },
      "element": {
        "type": "checkboxes",
        "action_id": "context",
        "options": [ { "text": { "type": "plain_text", "text": "Include the earlier messages in this thread" }, "value": "include" } ],
        "initial_options": [ { "text": { "type": "plain_text", "text": "Include the earlier messages in this thread" }, "value": "include" } ]
      }
    }
  ]
}
```

With workspaces on and at least one workspace listed for the person, a **Workspace** select sits
between Agent and Prompt, and the agent input gains `"dispatch_action": true`: a change of agent
reaches the gateway, which redraws the view (`views.update` with the view's `hash`) with the
new agent's preset under the next `block_id` revision, because Slack keeps a person's pick in an
input whose `block_id` stays. The select is optional; **No workspace** (`*none*`) comes first,
in a group of its own when the workspaces are grouped by Organization. The preset is the
person's last choice with the agent, else the agent's default workspace, else **No workspace**;
the hint names the agent when the preset is the last choice ("Last used with SRE Agent.") or its
default ("The default of SRE Agent."):

```json
{
  "type": "input",
  "block_id": "ask_agent_workspace.0",
  "optional": true,
  "label": { "type": "plain_text", "text": "Workspace" },
  "hint": { "type": "plain_text", "text": "Last used with SRE Agent." },
  "element": {
    "type": "static_select",
    "action_id": "workspace",
    "placeholder": { "type": "plain_text", "text": "Type to search or pick a workspace" },
    "option_groups": [
      { "label": { "type": "plain_text", "text": "Without a workspace" },
        "options": [ { "text": { "type": "plain_text", "text": "No workspace" }, "value": "*none*" } ] },
      { "label": { "type": "plain_text", "text": "giantswarm" },
        "options": [ { "text": { "type": "plain_text", "text": "platform · synced 3 h ago" }, "value": "platform" } ] }
    ],
    "initial_option": { "text": { "type": "plain_text", "text": "platform · synced 3 h ago" }, "value": "platform" }
  }
}
```

Submitting posts `<@U123> asked *SRE Agent*:` with the question quoted under it, under the
app's own identity: the words are the person's, so the agent's name and icon are not on the
message. It is a new root message for the slash command and a reply in the carried thread for
the shortcut. The question then runs as the thread's first turn, and the agent's answers keep
the agent's identity. With the box ticked,
the messages the thread held before that question message travel with the question as a labelled
part of the turn (see [the Slack adapter](channels-slack.md)); nothing about them is posted in the
thread.

Everything the picker cannot do is said privately to the invoker, through the interaction's
`response_url`, and nothing is posted in the channel:

| when | notice |
|---|---|
| the shortcut, in a thread that already talks to an agent | "This thread already talks to *Agent*. Reply in the thread to ask it…" |
| the shortcut, in a DM while DMs are not served | the DM redirect |
| the slash command, in a DM | "This command starts a conversation in a channel…" |
| the channel is not served | "This channel is not enabled yet…" |
| the caller is not signed in | "The agents are listed with your permissions, so sign in first…" (`login`) |
| the roster took longer than the trigger's 3-second life | "Listing the agents took too long for Slack's picker…" |
| the roster is unreachable, or empty | "The agents cannot be listed right now…" / "No agents are installed." |
| on submit: the picked agent no longer validates | "No agent named `…` is available…", with the current roster |
| on submit: the picked agent is installed but cannot run | "*Agent* is installed but cannot start a conversation right now: <reason>. Nothing was started." (+ the roster when it lists agents) |
| on submit: the picked workspace is not granted, or could not be checked | "Cannot start in workspace *name*: <reason>. Nothing was started; …", then "Your prompt:" and the prompt quoted |
| on submit: the bot is not in the channel and cannot join | "Invite the bot to the channel and try again." |

One more notice reaches the person who opened the conversation, as an ephemeral in the thread
rather than through the `response_url`, because by then the conversation is running:

| when | notice |
|---|---|
| the thread's earlier messages could not be read (`missing_scope`, `not_in_channel`, a timeout) | "The earlier messages in this thread could not be read (`<reason>`), so the agent sees only your question." |

One notice needs no conversation at all: it answers a message the gateway would otherwise
ignore. Ephemeral to the author, in the thread, and posted again on every such reply — it
writes nothing:

| when | notice |
|---|---|
| a reply without a mention, and not opening with someone else's mention, in a thread whose conversation ended after `routing.threadTTL` (the row is still in the store, at twice the lifetime) | "This conversation ended after 90 days without messages. Start a new one with `/swarmgeist`, or with *Ask an agent here* on a message in this thread." (the configured lifetime and slash command are named) |

## 13. Decision (a question to a person or a team)

Posted by a service through [`POST /decisions`](api.md#post-decisions): a direct message from the
app to a person, found by their email (`users.lookupByEmail`), or a message in a team's channel.
One message per decision:

- **Header:** the question. **Section:** the status quo.
- **One section per option:** the label in bold and its consequence, with a *Choose* button as
  its accessory. The recommended option's section starts with *Recommended* and its button is
  `primary`. A click answers at once: one row is one answer.
- **Actions:** *Answer in my own words* opens a modal (`callback_id: decision_answer`) with a
  required box of up to 3000 characters (`decision_answer_text.text`) and, when the decision has
  options, an optional select of them (`decision_answer_choice.choice`), so an answer can be an
  option with a condition, or none of them. **A plain reply in the message's thread** is the same
  answer in the person's own words.
- **Context:** `Due <date> · if unanswered: <default>`, and `For <team> · Asked by <asker> · note #<note>`.

Every answer calls the decision's answer tool through muster as the person who answered, with
its arguments plus `choice` (the option, 1-based) and `text` (their words), each only when given.
The tool decides whether that person may answer — the addressee, or a member of the team. The
rest is the team review's (section 10): an unlinked person is asked to sign in; a backend they
have not connected is connected from the answer, whose Connect prompt's click submits it again
once they signed in; a refusal or a
failure is a status line under the buttons, naming the person and the reason, and the decision
stays open; the first answer claims the record, so a second one is told who answered.

The answered message keeps the question and the status quo, drops the options and buttons, and
reads `Answered by <@U…> · <time>: <option> — <text>`. The asker closes the decision through
`POST /decisions/{id}/close` whenever it closes — answered here or elsewhere, at its due time, or
withdrawn — and the message is rewritten to `Answered · <time>: <text>`,
`Not answered by <due>; the default was applied: <default>` or `Withdrawn · <time>: <text>`. A
click on a closed decision is refused privately with how it closed.

A decision is named by its message, `<channel>-<ts>`: a click, the modal's `private_metadata` and
a thread reply all carry it. Only a reply under the bot's own message (`parent_user_id`) is looked
up, so a reply anywhere else costs no store read. The record lives in the routing store until the
due time plus seven days. Thread replies reach the gateway in a DM and in a public channel
(`message.im`, `message.channels`); a team decision in a private channel is answered by click or
modal only.

## 14. Conversation (a person's thread with an agent the gateway does not run)

Opened by a service through [`POST /conversations`](api.md#conversations) on behalf of an agent
that runs elsewhere, such as a person's guide on their own machine: a direct message from the app
to the person, found by their email. The opening message is the agent's text as a markdown
block, with a context line `<from> · reply in this thread`. The service posts every later message
of the agent's into that thread (`POST /conversations/{id}/messages`).

A reply of the person's in the thread calls the conversation's reply tool through muster as the
person, with its arguments plus `message`: the reply as an A2A Message, `messageId`
`slack-<channel>-<ts>` (a redelivered event is the same message), `role` `user`, `contextId` the
conversation's id, one text part and `metadata` `{"source": "slack", "conversation": <id>}`. The
tool decides whom the person may reach and the agent answers through the conversation's id.

- **Delivered:** an `:incoming_envelope:` reaction on the reply.
- **Refused** (no agent of theirs running, someone else's agent): a note in the thread,
  `Not delivered to <from>: <the tool's reason>`. Nothing is queued anywhere else.
- **Not reached** (muster or the tool did not answer): `Not delivered to <from>: it could not be
  reached right now. Send it again in a moment.`
- **Unlinked:** the sign-in card, in the thread; the person sends the reply again once signed in.
  A URL elicitation muster asks for first (a sign-in, an approval) is the elicitation prompt in
  the thread showing muster's link, the reply held behind it; *Done* accepts it, and muster
  delivers the reply once the person is done there. When muster still asks on that retry (an
  approval not decided yet), the prompt is posted again ("Not done yet: *muster* still asks
  ...") and the reply stays held for its *Done*; *Not now* declines it and nothing is sent.
- **Someone else** writing in the thread is told privately that it is the person's conversation;
  nothing is sent.

A conversation is named by its opening message, `<channel>-<ts>`, the same as a decision, so the
one lookup a reply under the bot's own message costs finds either. The record lives in the routing
store for 30 days after its latest message.

## 15. Workspace (a new thread's Session)

A normal thread message, posted when the initiator's message would start the thread's Session
and the initiator has workspaces to pick from
([Selecting a workspace](channels-slack.md#selecting-a-workspace)). The select's options are
grouped by Organization (`option_groups`, at most 100 workspaces), each labelled with the
workspace and the age of its last successful sync; an option's `value` is the JSON
`{"t":"<thread>","w":"<workspace>"}`, the button's `value` the thread. Picking an option is the
choice: there is no submit. Only the initiator's click counts; anyone else gets an ephemeral
"Only @…, who started this thread, chooses its workspace." On the choice the message is
rewritten to the start notice (a context block): "Working in workspace *platform*, chosen by
@…." or "Working without a workspace, chosen by @….". A workspace the workspace-manager does
not grant starts nothing, the prompt stays, and the initiator alone (an ephemeral in a channel,
a thread reply in a DM) is answered by the outcome:

- **Refused:** "You cannot start in workspace *secret*: you cannot read `giantswarm/vault`,
  `giantswarm/keys`. Nothing was started; pick another workspace or *No workspace*." with a
  **No workspace** button (`workspace_none`, the thread as its `value`).
- **Not synced yet** (or otherwise unavailable): "Cannot start in workspace *fresh* yet: <the
  workspace-manager's reason, else it has not synced yet>. Nothing was started; …" with the same button.
- **A provider not connected:** the connect prompt (section on connectors, `connector_connect` URL
  button to the sign-in link muster's `core_auth_login` answers for the server the provider's
  `signIn` names), one per provider: "Connect *gitlab* first, so the agent can work in workspace
  *gitlab-ws* as you. Your message is kept: once connected, pick the workspace again or mention
  me." A `signIn` that names no muster server gets the same text without a button, the
  workspace-manager's words added: "The workspace-manager says: muster holds no grant for
  provider gitlab: …." The initiator's next pick or mention in the
  thread authorizes it again and starts the Session with the held message.

A click on a prompt
that is no longer open is answered with an ephemeral that it expired, and the message is
rewritten to say so.

An agent with a default workspace gets no prompt: the default is chosen as the initiator's click
would. Granted, the thread gets a plain note, "Working in workspace *platform*, this agent's
default. To work in another, start with `/swarmgeist` and pick it." A provider to connect gets
the connect prompt above, ending "Your message is kept: once connected, mention me and it
runs." Any other denial is answered to the initiator alone, with no button and the message
quoted back, since nothing holds it: "You cannot start in workspace *platform*, this agent's
default: you cannot read `giantswarm/vault`, `giantswarm/keys`. Nothing was started; pick
another workspace or *No workspace* in `/swarmgeist`.\nYour prompt:\n>…" (likewise "Cannot
start in workspace *platform*, this agent's default, yet: …" and "Could not check workspace
*platform*, this agent's default, right now.").

```json
{
  "blocks": [
    { "type": "section", "text": { "type": "mrkdwn", "text": "*Which workspace should this conversation work in?*\nThe agent starts once <@U0INITIATOR> picks one, or *No workspace*. The list shows when each workspace last synced its repositories." } },
    {
      "type": "actions",
      "block_id": "workspace_prompt",
      "elements": [
        {
          "type": "static_select",
          "action_id": "workspace_select",
          "placeholder": { "type": "plain_text", "text": "Pick a workspace" },
          "option_groups": [
            { "label": { "type": "plain_text", "text": "acme" }, "options": [
              { "text": { "type": "plain_text", "text": "shop · never synced" }, "value": "{\"t\":\"THREAD_TS\",\"w\":\"shop\"}" }
            ] },
            { "label": { "type": "plain_text", "text": "giantswarm" }, "options": [
              { "text": { "type": "plain_text", "text": "platform · synced 3 h ago" }, "value": "{\"t\":\"THREAD_TS\",\"w\":\"platform\"}" }
            ] }
          ]
        },
        { "type": "button", "text": { "type": "plain_text", "text": "No workspace" }, "action_id": "workspace_none", "value": "THREAD_TS" }
      ]
    }
  ]
}
```

## Answering: click and reply

On a click, Slack POSTs a `block_actions` payload to `/channels/slack/interactions`. The
handler checks the clicking user is permitted, then routes by `action_id` (a
`team_review_approve` click is resolved against the review record instead, see section 10): approve/deny/chat
decide the tool call directly; a `hitl_choice_<i>` button commits that one choice; a
`hitl_submit` reads the selection(s) and a form's typed answers out of `state.values` (grouped per question for a form)
and resumes the paused task with one answer slot per question. An incomplete Submit is nudged
and the form is left pending. The prompt message is then rewritten in place. The question stays
and its controls go: one context line names the answer, who gave it and when (`gazelle ·
answered by <@U123> · <!date^…^{time}|21:43 UTC>`, several choices joined with commas). A form
keeps each question with its answer on the line under it ("No answer" when a typed reply had
no line for it), and its context line reads `Answered by <@U123> · …`. A long answer is cut so
the line stays within Slack's 3000-character limit, or Slack would refuse the whole rewrite. The approval card is rewritten the same way (section 1).

Every prompt can also be answered by a plain in-thread reply that does not open with a mention
of someone other than the bot, which maps free text to the same
structured decision. A question prompt's or an approval card's message is recorded when it
posts, so a typed answer rewrites it exactly like a click, and no live controls stay on a
decided prompt. A click that
finds its task gone or superseded keeps the prompt's text sections (the question, each question
of a form) and puts the note where the controls were.
