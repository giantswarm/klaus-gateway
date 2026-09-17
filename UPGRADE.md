# Upgrade notes

Breaking or operator-visible changes between releases, newest first. The
`CHANGELOG.md` lists every change; this file covers what an operator has to
do or decide.

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
