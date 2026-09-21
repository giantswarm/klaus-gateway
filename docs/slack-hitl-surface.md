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
may decide; an onlooker click is refused ephemerally. A thread is one shared session, so any
permitted user may answer a prompt it raised. The team review (section 10) is the exception:
it has no initiator, and any linked member of its team may decide.

## 1. Tool approval

The agent paused on a tool call that needs approval (`ToolName` is not `ask_user`). Approve
runs the call, Deny rejects it, Chat holds the task and swaps in a reply hint so the user can
ask a follow-up before deciding. `value` is the JSON `{"t":"<thread>","id":"<task>"}`;
the task binds the buttons to the prompt they render, so a click on a superseded
prompt is refused instead of answering a newer one.

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
        { "type": "button", "text": { "type": "plain_text", "text": "✅ Approve" }, "style": "primary", "action_id": "hitl_approve", "value": "{\"t\":\"THREAD_TS\",\"id\":\"TASK_ID\"}" },
        { "type": "button", "text": { "type": "plain_text", "text": "❌ Deny" }, "style": "danger", "action_id": "hitl_deny", "value": "{\"t\":\"THREAD_TS\",\"id\":\"TASK_ID\"}" },
        { "type": "button", "text": { "type": "plain_text", "text": "💬 Chat" }, "action_id": "hitl_chat", "value": "{\"t\":\"THREAD_TS\",\"id\":\"TASK_ID\"}" }
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
        { "type": "button", "text": { "type": "plain_text", "text": "Submit" }, "style": "primary", "action_id": "hitl_submit", "value": "{\"t\":\"THREAD_TS\",\"id\":\"TASK_ID\"}" }
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

Posted when a turn needs the user's token but they haven't linked their account. In a
channel it is ephemeral to that user, anchored by a thread notice that names nobody,
carries no link and is posted once per thread; in a DM it is a threaded message. The button
opens the linking flow. Once the link completes, a DM prompt is rewritten in place to the
signed-in confirmation and a channel prompt is confirmed with a fresh ephemeral. The link
expires after 15 minutes; a later message posts a fresh prompt. A DM prompt is rewritten to
say its link expired; a channel prompt cannot be rewritten, so the fresh ephemeral says the
earlier link expired instead. A turn that still has no user token is aborted rather than
run as the gateway identity.

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
replays their held message; No discards it and tells the newcomer (ephemerally) that the
owner declined. A click on a prompt whose thread state the gateway no longer holds (pod
restart, expiry) rewrites the prompt to say the approval expired. Each button's `value` is
the JSON `{"t":"<thread>","u":"<newcomer>"}`, since one initiator can have several pending
approvals at once.

A granted collaborator's turns run under the initiator's identity ("on your behalf"): the
gateway forwards the initiator's token so the thread stays one shared session, and the
collaborator's real identity rides along as attribution. See
[Threads and conversations](channels-slack.md#threads-and-conversations).

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
  reads `❌ *Denied* by <@U…> for team-bumblebee.`, the ask, the reason as a quote, then what the
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
  the call with its sign-in challenge — the tool's server holds no grant for the person — the
  clicker gets an ephemeral *Connect <server>* button. With a public base URL the link lands
  back on the gateway (`/connectors/complete`, the connector landing) and the approval is
  submitted again as the person, so one click and one consent are all it takes; without one the
  prompt says to click *Approve* again afterwards. A backend that still challenges after the
  landing is reported once, not looped.
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
        { "type": "button", "text": { "type": "plain_text", "text": "✅ Approve" }, "style": "primary", "action_id": "team_review_approve", "value": "{\"r\":\"REVIEW_ID\"}" },
        { "type": "button", "text": { "type": "plain_text", "text": "❌ Deny" }, "style": "danger", "action_id": "team_review_deny", "value": "{\"r\":\"REVIEW_ID\"}" },
        { "type": "button", "text": { "type": "plain_text", "text": "Open run" }, "action_id": "team_review_open", "url": "https://github.com/giantswarm/platform-manager/actions/runs/4242", "value": "{\"r\":\"REVIEW_ID\"}" }
      ]
    }
  ]
}
```

While an attempt is pending the message carries a context block under the actions, such as
`🔗 <@U…> is connecting *giantswarm-repo-manager* to approve as themselves.` or
`❌ <@U…>'s approval was not accepted: …`. After the approval the message reads
`✅ *Approved* by <@U…> for team-bumblebee.`, then the ask, then the tool's answer in italics,
with `<url|Open PR>` as a context block. The review is a record in the gateway's routing store
for seven days (see [`POST /reviews`](api.md#post-reviews)): on a store that outlives the process
a gateway restart changes nothing for the team, and a click on a review the gateway no longer
holds — seven days passed, or `routing.store: memory` restarted — rewrites the message to say it
expired. The completion state behind a review's *Connect* button is the process's own: after a
restart the landing says the link is gone, and the person clicks *Approve* again, now connected.

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
slash command (`/swarmgeist [question]`, whose text prefills the question box) and the **Ask an
agent here** message shortcut (⋯ menu → Apps, `callback_id: ask_agent_here`). The select lists
the live roster as the caller, the default agent preselected; `private_metadata` carries where
the picker was opened, how to answer the user privately, and — for the shortcut — the thread the
conversation starts in. In a channel, the shortcut's view carries a third block: a checkbox, ticked, offering the
thread's earlier messages to the agent. It names no count — counting would mean reading the thread
before `views.open`, and Slack invalidates the trigger three seconds after issuing it — and the
input is `optional`, so clearing the box still submits.

```json
{
  "type": "modal",
  "callback_id": "ask_agent",
  "private_metadata": "{\"c\":\"C123\",\"u\":\"U123\",\"r\":\"https://hooks.slack.com/actions/…\",\"t\":\"1699999999.000100\"}",
  "title": { "type": "plain_text", "text": "Ask an agent" },
  "submit": { "type": "plain_text", "text": "Ask" },
  "close": { "type": "plain_text", "text": "Cancel" },
  "blocks": [
    {
      "type": "input",
      "block_id": "ask_agent_agent",
      "label": { "type": "plain_text", "text": "Agent" },
      "element": {
        "type": "static_select",
        "action_id": "agent",
        "placeholder": { "type": "plain_text", "text": "Pick an agent" },
        "options": [ { "text": { "type": "plain_text", "text": "SRE Agent" }, "value": "kagent/sre-agent" } ],
        "initial_option": { "text": { "type": "plain_text", "text": "SRE Agent" }, "value": "kagent/sre-agent" }
      }
    },
    {
      "type": "input",
      "block_id": "ask_agent_question",
      "label": { "type": "plain_text", "text": "Question" },
      "element": { "type": "plain_text_input", "action_id": "question", "multiline": true, "max_length": 3000 }
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

Submitting posts the echo `💬 <@U123> asked *SRE Agent*:` with the question quoted, under the
agent's identity — a new root message for the slash command, a reply in the carried thread for
the shortcut — and runs the question as the thread's first turn. With the box ticked, the messages
the thread held before that echo travel with the question as a labelled part of the turn (see
[the Slack adapter](channels-slack.md)); nothing about them is posted in the thread.

Everything the picker cannot do is said privately to the invoker, through the interaction's
`response_url`, and nothing is posted in the channel:

| when | notice |
|---|---|
| the shortcut, in a thread that already talks to an agent | "This thread already talks to *Agent*. Reply in the thread to ask it…" |
| the shortcut, in a DM while DMs are not served | the DM redirect |
| the slash command, in a DM | "This command opens a conversation in a channel…" |
| the channel is not served | "I'm not enabled in this channel yet…" |
| the caller is not signed in | "I need to know who you are before I can list the agents…" (`/login`) |
| the roster took longer than the trigger's 3-second life | "Listing the agents took too long for Slack's picker…" |
| the roster is unreachable, or empty | "I can't list the available agents right now…" / "No agents are installed right now." |
| on submit: the picked agent no longer validates | "I don't know an agent named `…`", with the current roster |
| on submit: the picked agent is installed but cannot run | "*Agent* is installed but cannot start a conversation right now: <reason>. I haven't started anything." (+ the roster when it lists agents) |
| on submit: the bot is not in the channel and cannot join | "Invite me to the channel and try again." |

One more notice reaches the person who opened the conversation, as an ephemeral in the thread
rather than through the `response_url`, because by then the conversation is running:

| when | notice |
|---|---|
| the thread's earlier messages could not be read (`missing_scope`, `not_in_channel`, a timeout) | "I couldn't read the earlier messages in this thread (`<reason>`), so the agent only sees your question." |

One notice needs no conversation at all: it answers a message the gateway would otherwise
ignore. Ephemeral to the author, in the thread, and posted again on every such reply — it
writes nothing:

| when | notice |
|---|---|
| a reply without a mention in a thread whose conversation ended after `routing.threadTTL` (the row is still in the store, at twice the lifetime) | "This conversation ended after 90 days without messages. Mention me to start a new one." (the configured lifetime is named) |

## Answering: click and reply

On a click, Slack POSTs a `block_actions` payload to `/channels/slack/interactions`. The
handler checks the clicking user is permitted, then routes by `action_id` (a
`team_review_approve` click is resolved against the review record instead, see section 10): approve/deny/chat
decide the tool call directly; a `hitl_choice_<i>` button commits that one choice; a
`hitl_submit` reads the selection(s) out of `state.values` (grouped per question for a form)
and resumes the paused task with one answer slot per question. An incomplete Submit is nudged
and the form is left pending. The prompt message is then rewritten in place to show the chosen
answer.

Every prompt can also be answered by a plain in-thread reply, which maps free text to the same
structured decision — this is the only path on the web and CLI channels, which don't render
interactive widgets.
