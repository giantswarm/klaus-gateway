# HITL & interactive surface (Slack)

Every interactive prompt klaus-gateway can post to Slack (as the **Swarmgeist** app), when it
appears, and how the user answers it. The gateway receives a neutral stream of deltas from a
Klaus instance (over A2A); when a turn pauses for the user, the Slack adapter renders one of
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
| 2–20 | each 1–10 | any | each ≤75 | single **form** (one group per question) | one Submit |
| >20, or any question free-text / >10 / label >75 | — | — | — | numbered text + free-text reply | reply |

Generic (non-`ask_user`) tool approvals always render as Approve / Deny / Chat buttons.
Any prompt can also be answered by replying in-thread; a reply resolves the paused task the
same way a click does. Only a permitted user (the thread initiator or a granted collaborator)
may decide; an onlooker click is refused ephemerally.

## 1. Tool approval

The agent paused on a tool call that needs approval (`ToolName` is not `ask_user`). Approve
runs the call, Deny rejects it, Chat holds the task and swaps in a reply hint so the user can
ask a follow-up before deciding. `value` is the raw thread timestamp.

```json
{
  "blocks": [
    {
      "type": "section",
      "text": { "type": "mrkdwn", "text": "The agent wants to run *delete_cluster* on `prod-eu`." }
    },
    {
      "type": "actions",
      "elements": [
        { "type": "button", "text": { "type": "plain_text", "text": "✅ Approve" }, "style": "primary", "action_id": "hitl_approve", "value": "THREAD_TS" },
        { "type": "button", "text": { "type": "plain_text", "text": "❌ Deny" }, "style": "danger", "action_id": "hitl_deny", "value": "THREAD_TS" },
        { "type": "button", "text": { "type": "plain_text", "text": "💬 Chat" }, "action_id": "hitl_chat", "value": "THREAD_TS" }
      ]
    }
  ]
}
```

<img width="727" height="174" alt="image" src="https://github.com/user-attachments/assets/4aced0b9-64f5-49c0-aaab-63798a1956bf" />

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
        { "type": "button", "text": { "type": "plain_text", "text": "Submit" }, "style": "primary", "action_id": "hitl_submit", "value": "THREAD_TS" }
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
        { "type": "button", "text": { "type": "plain_text", "text": "Submit" }, "style": "primary", "action_id": "hitl_submit", "value": "THREAD_TS" }
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
is `hitl_choice_<index>` and its `value` is the JSON `{"t":"<thread>","c":<index>}`.

```json
{
  "blocks": [
    { "type": "section", "text": { "type": "mrkdwn", "text": "*Which migration plan do you want?*" } },
    {
      "type": "section",
      "text": { "type": "mrkdwn", "text": "Blue/green: stand up a parallel cluster, cut over DNS once healthy, keep the old one for fast rollback." },
      "accessory": { "type": "button", "text": { "type": "plain_text", "text": "Select" }, "action_id": "hitl_choice_0", "value": "{\"t\":\"THREAD_TS\",\"c\":0}" }
    },
    {
      "type": "section",
      "text": { "type": "mrkdwn", "text": "In-place: drain and upgrade nodes one at a time; lower cost, longer window, no parallel capacity." },
      "accessory": { "type": "button", "text": { "type": "plain_text", "text": "Select" }, "action_id": "hitl_choice_1", "value": "{\"t\":\"THREAD_TS\",\"c\":1}" }
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
        { "type": "button", "text": { "type": "plain_text", "text": "Submit" }, "style": "primary", "action_id": "hitl_submit", "value": "THREAD_TS" }
      ]
    }
  ]
}
```

<img width="720" height="336" alt="image" src="https://github.com/user-attachments/assets/fb858be8-0f7a-46df-b90b-6e3ecbf4d406" />


## 6. ask_user — multiple questions, single form

When every question is widget-renderable (1–10 choices, each label ≤75 runes) and there are
2–20 of them, the whole prompt renders as one form: a section + radio/checkbox group per
question, committed by a single Submit. Each group's `block_id` is `hitl_q_<question index>`
so the handler maps each selection back to its question. The Submit resumes only once every
question is answered; an incomplete Submit nudges and leaves the form pending. The `*bold*`
wrapping each question is added by the gateway.

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
    {
      "type": "actions",
      "elements": [
        { "type": "button", "text": { "type": "plain_text", "text": "Submit" }, "style": "primary", "action_id": "hitl_submit", "value": "THREAD_TS" }
      ]
    }
  ]
}
```

<img width="714" height="359" alt="image" src="https://github.com/user-attachments/assets/21e82e7c-0a78-429c-9e93-a684f527a6a3" />

## 7. Connector login (agent needs a backend the user hasn't connected)

Ephemeral (visible only to the user). Posted when a tool result carries a backend login URL
for a backend the agent can't yet use on the user's behalf. Connect opens the backend's
consent flow in the browser; Not now dismisses it (a per-user/per-backend cooldown suppresses
re-prompts). Both button `value`s carry the backend name; replace `https://…` with a real URL
for the Builder to accept it.

```json
{
  "blocks": [
    { "type": "section", "text": { "type": "mrkdwn", "text": "The agent can't use *github* for you yet. Connect your account once so those tools work." } },
    {
      "type": "actions",
      "elements": [
        { "type": "button", "text": { "type": "plain_text", "text": "Connect github" }, "style": "primary", "action_id": "connector_connect", "value": "github", "url": "https://example.com/connect" },
        { "type": "button", "text": { "type": "plain_text", "text": "Not now" }, "action_id": "connector_dismiss", "value": "github" }
      ]
    }
  ]
}
```

<img width="721" height="159" alt="image" src="https://github.com/user-attachments/assets/64649be1-74d6-4012-86d2-14f8d0998a84" />

## 8. OBO sign-in (act-as-user account linking)

Ephemeral. Posted when a turn needs the user's token but they haven't linked their account.
The button opens the linking flow; once it completes the ephemeral prompt is replaced in
place. A turn that still has no user token degrades: the tool call fails at the backend rather
than running as the gateway identity.

```json
{
  "blocks": [
    { "type": "section", "text": { "type": "mrkdwn", "text": "Sign in so I can act as you. Until you do, I can't run tools on your behalf." } },
    {
      "type": "actions",
      "elements": [
        { "type": "button", "text": { "type": "plain_text", "text": "Sign in" }, "style": "primary", "action_id": "obo_sign_in", "url": "https://example.com/login" }
      ]
    }
  ]
}
```

<img width="721" height="159" alt="image" src="https://github.com/user-attachments/assets/1d629999-8f4a-41be-ba10-70d2c3a0fa57" />

## 9. Access consent (a newcomer wants to instruct the agent in someone's thread)

Ephemeral, shown only to the thread initiator. Yes grants the newcomer (additively) and
replays their held message; No discards it. Each button's `value` is the JSON
`{"t":"<thread>","u":"<newcomer>"}`, since one initiator can have several pending approvals at
once.

```json
{
  "blocks": [
    { "type": "section", "text": { "type": "mrkdwn", "text": "Is <@U0NEWCOMER> allowed to instruct the agent to work on your behalf in this thread?" } },
    {
      "type": "actions",
      "elements": [
        { "type": "button", "text": { "type": "plain_text", "text": "✅ Yes" }, "style": "primary", "action_id": "access_allow", "value": "{\"t\":\"THREAD_TS\",\"u\":\"U0NEWCOMER\"}" },
        { "type": "button", "text": { "type": "plain_text", "text": "❌ No" }, "style": "danger", "action_id": "access_deny", "value": "{\"t\":\"THREAD_TS\",\"u\":\"U0NEWCOMER\"}" }
      ]
    }
  ]
}
```

<img width="721" height="159" alt="image" src="https://github.com/user-attachments/assets/8249895e-3da1-41e6-9e23-1d42bb5a26b2" />

## Answering: click and reply

On a click, Slack POSTs a `block_actions` payload to `/channels/slack/interactions`. The
handler checks the clicking user is permitted, then routes by `action_id`: approve/deny/chat
decide the tool call directly; a `hitl_choice_<i>` button commits that one choice; a
`hitl_submit` reads the selection(s) out of `state.values` (grouped per question for a form)
and resumes the paused task with one answer slot per question. An incomplete Submit is nudged
and the form is left pending. The prompt message is then rewritten in place to show the chosen
answer.

Every prompt can also be answered by a plain in-thread reply, which maps free text to the same
structured decision — this is the only path on the web and CLI channels, which don't render
interactive widgets.
