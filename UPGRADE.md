# Upgrade notes

Breaking or operator-visible changes between releases, newest first. The
`CHANGELOG.md` lists every change; this file covers what an operator has to
do or decide.

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
cluster-backed too (`routing.store: configmap` or `crd`) before
`replicaCount` goes above one.
