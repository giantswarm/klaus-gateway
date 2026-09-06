# klaus-gateway

Channel and routing gateway in front of klaus instances; uses agentgateway as the LLM/MCP/A2A data plane

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
| routing.store | string | `"memory"` |  |
| routing.boltPath | string | `"/var/lib/klaus-gateway/routes.bolt"` |  |
| routing.defaultTTL | string | `"24h"` |  |
| routing.autoCreate | bool | `false` |  |
| crd.install | bool | `true` |  |
| controller.enabled | bool | `false` |  |
| lifecycle.driver | string | `"operator"` |  |
| lifecycle.klausctlBin | string | `""` |  |
| lifecycle.operatorMCPURL | string | `""` |  |
| lifecycle.operatorMCPToken | string | `""` |  |
| lifecycle.staticInstances | string | `""` |  |
| upstream.url | string | `""` |  |
| upstream.agentgatewayURL | string | `""` |  |
| observability.otlpEndpoint | string | `""` |  |
| podLabels | object | `{}` |  |
| podSecurityContext.seccompProfile.type | string | `"RuntimeDefault"` |  |
| securityContext.allowPrivilegeEscalation | bool | `false` |  |
| securityContext.readOnlyRootFilesystem | bool | `true` |  |
| securityContext.seccompProfile.type | string | `"RuntimeDefault"` |  |
| securityContext.capabilities.drop[0] | string | `"ALL"` |  |
| agentgateway.enabled | bool | `false` |  |
| agentgateway.gatewayClassName | string | `"agentgateway"` |  |
| agentgateway.gateway.create | bool | `true` |  |
| agentgateway.gateway.parentRefs | list | `[]` |  |
| agentgateway.gateway.listeners[0].name | string | `"http"` |  |
| agentgateway.gateway.listeners[0].port | int | `80` |  |
| agentgateway.gateway.listeners[0].protocol | string | `"HTTP"` |  |
| agentgateway.gateway.listeners[0].allowedRoutes.namespaces.from | string | `"Same"` |  |
| agentgateway.gateway.annotations | object | `{}` |  |
| agentgateway.routes.chatAndMcp.enabled | bool | `true` |  |
| agentgateway.routes.chatAndMcp.prefixes[0] | string | `"/v1/"` |  |
| agentgateway.routes.chatAndMcp.prefixes[1] | string | `"/mcp"` |  |
| agentgateway.routes.cli.enabled | bool | `false` |  |
| agentgateway.routes.cli.prefixes[0] | string | `"/cli/v1/"` |  |
| agentgateway.routes.obo.enabled | bool | `false` |  |
| agentgateway.routes.obo.prefixes[0] | string | `"/auth/slack/"` |  |
| agentgateway.routes.directInstance.enabled | bool | `false` |  |
| agentgateway.policy.enabled | bool | `false` |  |
| agentgateway.policy.jwt.enabled | bool | `false` |  |
| agentgateway.policy.jwt.mode | string | `"Strict"` |  |
| agentgateway.policy.jwt.issuer | string | `""` |  |
| agentgateway.policy.jwt.audiences | list | `[]` |  |
| agentgateway.policy.jwt.jwks.mode | string | `"remote"` |  |
| agentgateway.policy.jwt.jwks.inline | string | `""` |  |
| agentgateway.policy.jwt.jwks.remote.jwksPath | string | `"/.well-known/jwks.json"` |  |
| agentgateway.policy.jwt.jwks.remote.cacheDuration | string | `"5m"` |  |
| agentgateway.policy.jwt.jwks.remote.backendRef.group | string | `""` |  |
| agentgateway.policy.jwt.jwks.remote.backendRef.kind | string | `"Service"` |  |
| agentgateway.policy.jwt.jwks.remote.backendRef.name | string | `""` |  |
| agentgateway.policy.jwt.jwks.remote.backendRef.namespace | string | `""` |  |
| agentgateway.policy.jwt.jwks.remote.backendRef.port | int | `443` |  |
| agentgateway.backendsExample.enabled | bool | `false` |  |
| agentgateway.backendsExample.name | string | `"klaus-instance-example"` |  |
| agentgateway.backendsExample.static.host | string | `"klaus-instance-example.default.svc.cluster.local"` |  |
| agentgateway.backendsExample.static.port | int | `8080` |  |
| a2a.enabled | bool | `false` |  |
| a2a.defaultAgent | string | `"sre-agent"` |  |
| a2a.url | string | `""` |  |
| a2a.tokenPath | string | `""` |  |
| a2a.fallbackIconUrlTemplate | string | `""` |  |
| a2a.saToken.enabled | bool | `false` |  |
| a2a.saToken.audience | string | `"kagent"` |  |
| a2a.saToken.mountPath | string | `"/var/run/secrets/kagent/token"` |  |
| a2a.saToken.expirationSeconds | int | `3600` |  |
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
| cli.enabled | bool | `false` |  |
| web.enabled | bool | `false` |  |
| obo.enabled | bool | `false` |  |
| obo.musterUrl | string | `""` |  |
| obo.callbackBaseUrl | string | `""` |  |
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
| global.podSecurityStandards.enforced | bool | `false` |  |
| nodeSelector | object | `{}` |  |
| tolerations | list | `[]` |  |
| affinity | object | `{}` |  |
