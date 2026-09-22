# klaus-gateway

Slack channel gateway for the Agent Platform's kagent agents

**Homepage:** <https://github.com/giantswarm/klaus-gateway>

## Maintainers

| Name | Email | Url |
| ---- | ------ | --- |
| Giant Swarm |  | <https://www.giantswarm.io> |

## Source Code

* <https://github.com/giantswarm/klaus-gateway>

## Values

| Key | Type | Default | Description |
|-----|------|---------|-------------|
| enabled | bool | `true` |  |
| agentgatewayRoute | object | `{}` |  |
| web | object | `{}` | Ignored since the Slack-only release; accepted because the agent-platform umbrella still forwards it; removed in a later minor. |
| cli | object | `{}` | Ignored since the Slack-only release; accepted because the agent-platform umbrella still forwards it; removed in a later minor. |
| lifecycle | object | `{}` | Ignored since the Slack-only release; accepted because the agent-platform umbrella still forwards it; removed in a later minor. |
| upstream | object | `{}` | Ignored since the Slack-only release; accepted because the agent-platform umbrella still forwards it; removed in a later minor. |
| agentgateway | object | `{}` | Ignored since the Slack-only release; accepted because the agent-platform umbrella still forwards it; removed in a later minor. |
| name | string | `"klaus-gateway"` |  |
| serviceType | string | `"managed"` |  |
| fullnameOverride | string | `""` |  |
| image.registry | string | `"gsoci.azurecr.io"` |  |
| image.name | string | `"giantswarm/klaus-gateway"` |  |
| image.tag | string | `""` |  |
| image.pullPolicy | string | `"IfNotPresent"` |  |
| replicaCount | int | `1` |  |
| logLevel | string | `"info"` |  |
| pod.user.id | int | `65532` |  |
| pod.group.id | int | `65532` |  |
| resources.limits.cpu | string | `"500m"` |  |
| resources.limits.memory | string | `"256Mi"` |  |
| resources.requests.cpu | string | `"100m"` |  |
| resources.requests.memory | string | `"128Mi"` |  |
| serviceAccount.create | bool | `true` |  |
| serviceAccount.annotations | object | `{}` |  |
| server.port | int | `8080` |  |
| admin.port | int | `8081` |  |
| metrics.enabled | bool | `true` |  |
| serviceMonitor.enabled | bool | `true` |  |
| serviceMonitor.interval | string | `"60s"` |  |
| serviceMonitor.scrapeTimeout | string | `"45s"` |  |
| serviceMonitor.labels | object | `{}` | Labels on the ServiceMonitor, beside the chart's own. The Giant Swarm observability platform routes a scrape to a Mimir tenant by `observability.giantswarm.io/tenant`; a monitor without it writes to no tenant. |
| routing.store | string | `"memory"` |  |
| routing.boltPath | string | `"/var/lib/klaus-gateway/routes.bolt"` |  |
| routing.defaultTTL | string | `""` | Ignored since the Slack-only release; accepted because the agent-platform umbrella still forwards it; removed in a later minor. It was the TTL of a Klaus route entry; a thread's lifetime is threadTTL. |
| routing.threadTTL | string | `""` |  |
| routing.autoCreate | bool | `false` | Ignored since the Slack-only release; accepted because the agent-platform umbrella still forwards it; removed in a later minor. It created a Klaus instance on a route miss; there is no Klaus path left. |
| routing.valkey.url | string | `""` |  |
| routing.valkey.username | string | `""` |  |
| routing.valkey.existingSecret | string | `""` |  |
| routing.valkey.passwordKey | string | `"valkey-password"` |  |
| routing.valkey.db | int | `0` |  |
| routing.valkey.tls.enabled | bool | `false` |  |
| routing.valkey.tls.serverName | string | `""` |  |
| routing.valkey.keyPrefix | string | `""` |  |
| routing.valkey.timeout | string | `"2s"` |  |
| observability.otlpEndpoint | string | `""` |  |
| observability.otlpHeaders | object | `{}` | Headers sent with every trace export, e.g. the tenant of a multi-tenant gateway: `X-Scope-OrgID: giantswarm`. Rendered as --otel-otlp-headers. |
| podAnnotations | object | `{}` | Annotations on the pod template (merged over the chart's own). The agent platform sets `karpenter.sh/do-not-disrupt: "true"` here so Karpenter's consolidation works around the pod that holds the channel connections and the in-flight A2A streams instead of evicting it. |
| podLabels | object | `{}` | Labels on the pod template. |
| podDisruptionBudget | object | `{"enabled":false,"maxUnavailable":null,"minAvailable":1,"unhealthyPodEvictionPolicy":""}` | PodDisruptionBudget on the gateway pods. Off by default: with `replicaCount: 1` a `minAvailable: 1` budget refuses every voluntary eviction (node drains wait for the drain timeout), which is the intended guard for a single replica that carries live Slack turns, but a deliberate choice. Set exactly one of `minAvailable` / `maxUnavailable` (int or percentage); `unhealthyPodEvictionPolicy: AlwaysAllow` lets a pod that is not Ready be evicted regardless, so a crash-looping gateway never wedges a drain. |
| terminationGracePeriodSeconds | int | `45` | Seconds the kubelet gives the pod to stop before it is killed. The shutdown drains the HTTP servers (up to 15 s), then stops the channel adapters (up to 15 s more), which is when a Slack turn cut short posts its restart notice and clears its progress reaction; the rest is margin for the client and store closes. Lower than the drain plus the stop and a turn interrupted by a restart ends in silence. |
| podSecurityContext.seccompProfile.type | string | `"RuntimeDefault"` |  |
| securityContext.allowPrivilegeEscalation | bool | `false` |  |
| securityContext.readOnlyRootFilesystem | bool | `true` |  |
| securityContext.seccompProfile.type | string | `"RuntimeDefault"` |  |
| securityContext.capabilities.drop[0] | string | `"ALL"` |  |
| a2a.enabled | bool | `false` |  |
| a2a.defaultAgent | string | `"sre-agent"` |  |
| a2a.url | string | `""` |  |
| a2a.namespace | string | `"kagent"` |  |
| a2a.caSecret | string | `""` |  |
| a2a.caFile | string | `""` |  |
| a2a.tokenPath | string | `""` | Ignored since the Slack-only release; accepted because the agent-platform umbrella still forwards it; removed in a later minor. It named a Bearer token file for the Klaus-instance paths, which are gone. |
| a2a.fallbackIconUrlTemplate | string | `""` |  |
| a2a.saToken | object | `{}` | Ignored since the Slack-only release; accepted because the agent-platform umbrella still forwards it; removed in a later minor. It projected a ServiceAccount token for the Klaus-instance paths, which are gone; the pod mounts nothing for it any more. |
| slack.enabled | bool | `false` |  |
| slack.mode | string | `"events"` |  |
| slack.secretName | string | `""` |  |
| slack.dmMode | string | `""` |  |
| slack.channelMode | string | `""` |  |
| slack.channelAllowlist | list | `[]` |  |
| slack.dropStale | bool | `false` |  |
| slack.progress.mode | string | `""` |  |
| slack.progress.emojis.working | string | `""` |  |
| slack.progress.emojis.done | string | `""` |  |
| slack.progress.emojis.failed | string | `""` |  |
| slack.progress.clearReactionOnDone | string | `nil` |  |
| slack.botToken | string | `""` |  |
| slack.signingSecret | string | `""` |  |
| slack.appToken | string | `""` |  |
| obo.enabled | bool | `false` |  |
| obo.musterUrl | string | `""` |  |
| obo.callbackBaseUrl | string | `""` |  |
| obo.store | string | `"bolt"` |  |
| obo.storeSecretName | string | `""` |  |
| obo.storePath | string | `""` |  |
| obo.persistence.enabled | bool | `false` |  |
| obo.persistence.size | string | `"64Mi"` |  |
| obo.persistence.accessMode | string | `"ReadWriteOnce"` |  |
| obo.persistence.storageClass | string | `""` |  |
| obo.persistence.existingClaim | string | `""` |  |
| obo.existingSecret | string | `""` |  |
| obo.stateKey | string | `""` |  |
| obo.storeKey | string | `""` |  |
| obo.connectors.enabled | bool | `false` |  |
| reviews.enabled | bool | `false` |  |
| reviews.audience | string | `"klaus-gateway"` |  |
| reviews.allowedCallers[0] | string | `"system:serviceaccount:giantswarm-platform-manager:giantswarm-platform-manager"` |  |
| global.podSecurityStandards.enforced | bool | `false` |  |
| nodeSelector | object | `{}` |  |
| tolerations | list | `[]` |  |
| affinity | object | `{}` |  |
