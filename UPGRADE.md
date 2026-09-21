# Upgrade notes

Breaking or operator-visible changes between releases, newest first. The
`CHANGELOG.md` lists every change; this file covers what an operator has to
do or decide.

## Next — the agent's steps move inside the Slack reply

A turn's tool calls are now steps of Slack's native task list, attached to the reply message
itself, instead of the gateway's own progress messages. Nothing to configure — no chart value,
no flag, no new scope — and no way to turn it off: **rollback is the previous image**.

What goes away from the thread: the separate hourglass message (`⏳ filter_tools… · step 3`)
that was edited on every tool call, and the one-line receipt it collapsed into
(`🛠️ 8 steps · x_kubernetes_list, …`). What replaces them: a step list inside the reply, with
one entry per call — "Listing the available tools", "Kubernetes list" — that turns from running
to done, or to an error when the tool failed, and that Slack collapses once the answer is
complete. One message per answer instead of two, in plain language rather than API names.

`/details` still decides how much is shown: `off` renders no steps at all, `on` (the default)
shows the titles, `full` adds the tool's arguments and a result preview on each step. `full` no
longer posts the separate JSON activity messages — **Inspect agent steps** (⋯ menu → Apps) is
the audit view, with the fuller payloads, and it is unchanged and still records at `on` and
`full` alike.

What to watch: nothing new. The steps ride the same `chat.appendStream` calls as the answer
text, so a tool-heavy turn no longer costs one `chat.update` per call, and
`klaus_gateway_slack_stream_total` and `klaus_gateway_slack_rate_limited_total` keep their
meaning. One cosmetic edge: a turn continued after a hard gateway restart closes the step that
was running when the process died under a plain "Step N", because the delivery record carries
the count, not the title.

## Next — Slack replies are streamed (chat.startStream)

Agent replies are written with Slack's streaming API instead of a message edited every 250 ms:
`chat.startStream` opens the answer on the turn's first text, `chat.appendStream` adds what has
accumulated once a second, and `chat.stopStream` closes it. The working indicator is still cleared
by the `agents.sessions.setStatus` call every turn ends with, as on the previous release.
Nothing to configure — no chart value, no flag, no new scope (`chat:write` already covers the
three methods) — and no way to turn it off: **rollback is the previous image**.

What people see change: the answer grows in place while Slack animates it; text lands on word
boundaries, so half-written words no longer flicker; an answer over 12,000 characters still
continues in a follow-up message; the working indicator clears together with the answer's last
words; and in text-progress mode the `_thinking…_` placeholder is deleted once the answer has a
message of its own. Two continuation cases read differently: a turn continued after a hard
restart goes on in the message the previous process left open (after a graceful restart that
message was already closed, so the continuation opens a new one), and a turn that resumes after
an approval prompt writes its continuation as a **second message** instead of editing the first.

What to watch: `chat.startStream` and `chat.stopStream` are **tier 2**, about 20 calls a minute
for the whole app, which bounds a workspace to roughly 20 turn starts a minute — the appends are
tier 4 and have plenty of headroom, and the two session-status calls per turn are unchanged. `/metrics` exposes `klaus_gateway_slack_stream_total{event}`
with `started`, `stopped`, `stopped_by_user` and `recovered`; the `started` rate against that
ceiling is the number to alert on, and a rising `recovered` means Slack is closing streams under
the gateway. Expect `stopped_by_user` to stay at zero: it is defensive, and a Stop press was
observed to leave the gateway's own `chat.stopStream` succeeding normally rather than answering
with that code. A 429 is still paced by `Retry-After`: one mid-answer costs nothing, since the
text it did not deliver rides the next flush, and only a call given up after the retries — on
the flush that closes the answer — can leave the reply incomplete. Those 429s are now counted
as `klaus_gateway_slack_rate_limited_total{method,outcome}`, `retried` when the pause absorbed
one and `exhausted` when the call was given up, so throttling that used to leave no trace can
be alerted on.

## Next — a Slack thread's row lives twice as long

A Slack thread's conversation still ends after `routing.threadTTL` (`--thread-ttl`, 90 days by
default): the agent, the initiator, the grants and the AgentInstance binding all read as absent
from then on, and the next mention starts the thread over. What changed is the row's own expiry,
which is now **twice** the lifetime (180 days by default; `0` still never expires). The gateway
uses that second half to tell a thread whose conversation ended from a thread it was never in, so
a reply without a mention in one of the former gets one private line instead of silence.

**No action needed.** One thing to know if you size the store: on Valkey the row is one key whose
expiry is that lifetime, so the keys of threads nobody writes in are held twice as long as before
— the row is a few hundred bytes, and the count is the number of Slack threads the gateway has
ever answered in, not a per-message growth. Set `routing.threadTTL` lower if that matters; the
notice then names the lifetime you set. A thread's row adopts the configured lifetime on its next
message, so a change to `routing.threadTTL` reaches the threads people keep using.

**`routing.defaultTTL` no longer governs a Slack thread's row.** A Slack thread's row is the same
row as its Klaus-instance route (the key's user slot is empty, because a thread is shared by its
participants), and until now a write to it kept the TTL the router had stamped —
`routing.defaultTTL`, 24 hours by default. It is now stamped with twice the thread lifetime like
every other thread row, so on the Klaus (non-kagent) path a Slack row that lived 24 hours lives
180 days. That also corrects a fault of its own: the thread's initiator and the grants they gave
died after 24 hours, and the thread then asked for consent again while people were still talking
in it. `routing.defaultTTL` still governs the web and CLI routes, which are keyed per user.

## Next — two new Slack scopes for the thread a conversation opens in

A conversation that opens inside an existing thread now hands that thread's earlier messages to
the agent, which means reading the thread: `channels:history` (already granted) covers public
channels, and the two new scopes in `deploy/slack/manifest.yaml` — `groups:history` and
`mpim:history` — private channels and group DMs. A 1:1 DM is never read this way, so nothing
changes for the assistant pane. Slack applies added scopes to an existing install only on
re-install, so **re-install the three Swarmgeist apps** (api.slack.com/apps → the app → OAuth &
Permissions, or re-import the manifest and then Install App) once this release is out. Nothing
breaks in the meantime: a thread in a private channel or a group DM answers `missing_scope`, the
conversation opens and runs as before, and the person who opened it gets one ephemeral saying the
agent only sees their question. Public channels need no re-install.

There is no opt-out per channel. The person starting the session decides: the picker's checkbox is
ticked by default and one click clears it, the transcript is labelled with who shared it, and the
echo is posted in the thread in the open, as before.

## Next — the "Ask an agent here" Slack shortcut

The Slack app gains a second message shortcut, `ask_agent_here`, which starts a conversation with
a chosen agent inside any message's thread. Slack does not apply manifests by itself, so an
existing app needs it added by hand at api.slack.com/apps (Features → Interactivity & Shortcuts →
Shortcuts → Create New Shortcut → On messages) with callback ID `ask_agent_here`; re-importing
`deploy/slack/manifest.yaml` does the same. No reinstall and no new scope: `commands` is already
granted on any install that carries the existing shortcut. The display name is per app — the
gateway routes on the callback ID alone — so name it whatever suits your workspace. Slack lists a
shortcut under "Connect to apps" in a message's ⋯ menu only after a person has used it once;
until then it sits behind "More message shortcuts…".

## Next — Slack thread state lives in the routing store

Every Slack thread now has one row in the routing store: its agent, its initiator, the
collaborators the initiator allowed, its AgentInstance binding, and the task in flight on it. Run
`routing.store: valkey` (see 1.6.0): on `memory` the state is lost on every restart and the
gateway logs a warning at start. Nothing is migrated: a thread that exists at the upgrade gets one
fresh start on its next reply, and its initiator is whoever replies first. The row has one sliding
lifetime, `routing.threadTTL` (`--thread-ttl`), 90 days by default and `0` to never expire,
refreshed by every turn; after it the thread is forgotten and the next mention starts it over:
its author becomes the initiator and no grant carries over. The agent's session is the
controller's: its create is idempotent per person and thread, so the same person may get the
earlier session back while the controller still holds it. Decide whether 90 days suits your
workspace before upgrading. The 24-hour
access window is gone with it: while a thread lives, the initiator and the people they allowed
keep replying without mentioning the bot again, and their grants no longer lapse after a day of
silence.
`/agent <name> <question>` now also opens a conversation as a reply inside an existing thread.
The "🚀 Bringing in *Agent* to help…" launch announcement is gone: it was posted under the
agent's own name and read as the agent introducing itself. The agent's first reply, under the
agent's name, is now the first message of a conversation; the progress reaction or the working
indicator still shows at once that the message was heard.

The AgentInstance itself is unaffected by any of this: the kagent request id is derived from the
channel, the thread and the agent, never from the store key, so a thread that exists at the
upgrade gets its AgentInstance back — the same conversation — on its first turn afterwards; only
its initiator resets, as above. What does not carry over is the old binding row: a 1.10.0 gateway
wrote it at a five-part routing key (`…|<agentRef>`), and this release reads and writes only the
four-part key above, so those old keys are never looked at again. They carry no TTL of their own,
so Valkey keeps them until removed by hand; they are harmless, and doing so is optional, for
example `valkey-cli --scan --pattern 'klaus-gateway:route:*' | awk -F'|' 'NF==5' | xargs -r
valkey-cli del` (adapt to the installation's Valkey auth).

## Next — the crd and configmap routing stores are gone

`routing.store: crd` and `routing.store: configmap` no longer exist; the gateway refuses to start
with `invalid --store`. The `controller.enabled` and `crd.install` values are gone too; a values
file that still sets them fails the chart's schema. Switch to `routing.store: valkey` (see 1.6.0)
before upgrading. The ChannelRoute CRD was a chart template: the upgrade deletes the
CustomResourceDefinition and with it every `ChannelRoute` object. No installation ran these stores.

## 1.10.0 — turn records, turn metrics, one trace per turn, the token refresh off the turn

Nothing to do for the metrics and the records: `/metrics` gains
`klaus_gateway_turn_total` and `klaus_gateway_turn_phase_seconds` (a handful
of series per channel; a ServiceMonitor already scraping the admin port picks
them up), and every turn logs a `turn_complete` record next to the
`turn_dispatch` it already logged.

Traces are exported when `observability.otlpEndpoint` is set, as before —
but the shape of a trace changed: the gateway's `<channel>.turn` span is now
the root and the kagent controller's `SendStreamingMessage` trace continues
it, so a Tempo search for `service.name=klaus-gateway` finds the whole turn
down to the actor. Two knobs are new: the endpoint may be a URL (its scheme
decides TLS; a bare `host:port` stays plaintext as before) and
`observability.otlpHeaders` adds headers to every export. On the agent
platform the meta chart sets both to the platform's OTLP gateway and its
tenant header by default, following its observability answer the way
kagent's exporters do; an installation that must not export sets
`klausGateway.observability.enabled: false` there.

The proactive token refresh changes the gateway's traffic to muster: one
`POST /oauth/token` per linked person who spoke in the last 48 hours, per
id_token lifetime, a few minutes before expiry — instead of the same call on
that person's first message after expiry. The count is the same, the
timing moves off the person's turn. A gateway that is restarted forgets whom
it served; the first message after a restart refreshes on the turn once, as
before.

## 1.6.0 — a routing store for installations (`routing.store: valkey`)

Every installation so far ran `routing.store: memory`: a gateway restart
dropped every thread's binding to its agent instance (the next reply started
a fresh conversation) and, since 1.5.0, the record of the turn in flight
whose result the restarted gateway would otherwise deliver. The chart's
cluster-backed stores were not a way out — `configmap` holds the whole table
in one object the chart grants no access to, `crd` needs the embedded
controller and cluster-scoped RBAC.

1.6.0 adds Valkey as a routing store: one key per thread, the entry's TTL as
the key's expiry, no volume, no API-server access. The agent platform already
runs a Valkey for muster's token store, so the switch is a values change:

```yaml
routing:
  store: valkey
  valkey:
    url: muster-valkey:6379
    existingSecret: agent-platform-secrets   # holds valkey-password
```

The gateway pod needs egress to the Valkey pods on 6379; on a Cilium
installation that is a connectivity-chart rule, not a gateway value. Every
Valkey call is bounded by `routing.valkey.timeout` (2 s): an outage fails the
turn with an error in the thread and fails readiness, both recover with the
server. Existing bindings are not migrated — the first reply in each thread
after the switch starts a fresh conversation once, as every restart did before.

`values.yaml` now states which stores are meant for installations (`valkey`,
`crd`) and what `bolt` needs to be durable (a mounted volume, which the chart
does not provide).

## 1.3.0 — the OBO link store moves off the volume (`obo.store: secret`)

The Slack on-behalf-of link store (`obo.storePath`, the encrypted bolt file)
so far lived on a ReadWriteOnce PersistentVolumeClaim (`obo.persistence`).
That binds the single gateway pod to one node: when the node is lost, the
replacement pod waits in `ContainerCreating` on `Multi-Attach error … Volume
is already exclusively attached to one node` until the node object is gone
(about four minutes on EBS), and a second replica is impossible.

1.3.0 adds a Kubernetes Secret as link-store backend. Every linked Slack user
is one entry of a single Secret in the release namespace, sealed with
`store-key` exactly as the bolt file sealed it, so nothing about the
encryption or the keys changes. The Deployment then needs no volume and no
`Recreate` strategy: a replacement pod is Ready on any node the moment it is
scheduled.

### The switch

```yaml
obo:
  store: secret          # default: bolt
  # storeSecretName: ""  # default <release>-obo-links, created by the chart
```

`store: bolt` (the default) leaves everything as it was; `obo.persistence`,
`existingClaim` and `storePath` keep their meaning.

### RBAC the Secret backend needs

The chart renders the link Secret (empty, `helm.sh/resource-policy: keep` so
an uninstall or reinstall never drops people's sign-ins) plus a `Role` and
`RoleBinding` that grant the gateway's ServiceAccount `get`, `update` and
`patch` on exactly that one Secret. Nothing else in the namespace becomes
readable. If you name a Secret you manage yourself (`obo.storeSecretName`),
the chart renders only the Role and RoleBinding for it; create the Secret
(type `Opaque`, no data) before the rollout, the gateway does not create it.

The gateway checks the Secret once at start and refuses to run when it is
missing or unreadable (log: `obo secret store: read secret …: forbidden`),
rather than treating every user as unlinked. With `serviceAccount.create:
false` bind the Role to your own ServiceAccount.

### Migration from the bolt file

The gateway imports the bolt file on the first start with the Secret backend
when `obo.storePath` is set and the file is mounted: every link the Secret
does not hold yet is copied in one write, the file is opened read-only and
left untouched, and only counts are logged (`imported links from bolt file
… imported=N total=N`). An entry already in the Secret wins, so the import
is safe to repeat and never overwrites a refresh token rotated after the
file was last written. Nobody has to sign in again.

Two rollouts, so the volume is still there for the import:

1. Set `obo.store: secret`; keep `obo.storePath` and `obo.persistence` as
   they are. The chart keeps mounting the claim (read-only now) and the
   `Recreate` strategy for this rollout; the gateway imports the links and
   serves from the Secret from then on. Check the log line and
   `kubectl get secret <release>-obo-links -o jsonpath='{.data}' | jq 'keys'`
   (the keys are Slack user IDs; the values are sealed).
2. Set `obo.persistence.enabled: false` and drop `obo.storePath`. The chart
   stops rendering the claim and the `Recreate` strategy; Helm deletes the
   `PersistentVolumeClaim` (`existingClaim` users keep theirs) and the pod
   rolls without a volume.

A fresh installation sets `store: secret` with no `storePath` and skips the
import. Rolling back to `store: bolt` before step 2 finds every link where
it was; after step 2 the file is gone with the claim and users link again.

### Multiple replicas

Writers use the Secret's `resourceVersion` for optimistic concurrency, so a
second replica can share the link store. The routing store still has to be
cluster-backed too (`routing.store: valkey`) before
`replicaCount` goes above one.
