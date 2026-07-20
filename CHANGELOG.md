# Changelog

All notable changes to this project will be documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.0.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).



## [Unreleased]

### Added

- Slack `/details on|off|full` command controls whether the agent's tool activity is shown in a thread. On by default; `off` quiets it, `full` also shows tool results. Rendered as fenced code blocks so Slack collapses long payloads. The setting applies to the whole thread.
- Slack `/usage` command reports token counts for the last turn and the accumulated session total, on demand. When the kagent REST API exposes the agent's model (declarative agents), the report also names the model and provider; a BYO agent omits the line.
- Slack `/stop` on a thread paused at an approval prompt resolves the pending tool call as a structured rejection (same as replying "stop"), instead of replying "Stopped." while leaving the prompt armed.
- Slack posts a "starting fresh" notice when an `@`-mention or DM reply continues a thread this gateway has no in-memory record of (e.g. after a restart) and its kagent session no longer exists, instead of silently losing context. Checked at most once per thread; never blocks the turn.
- `--a2a-rest-url` flag and `KLAUS_GATEWAY_A2A_REST_URL` env var set the kagent controller REST base URL used by the resume check. When unset it is derived from `--a2a-url`.

### Changed

- Slack OBO: the browser-facing sign-in outcomes at `/auth/slack/link` and `/auth/slack/callback` now render a branded, responsive light/dark HTML page (embedded via `//go:embed`), adapted from the platform gateway-api error template. This replaces the bare inline success HTML and the plain-text error responses, so both success and error cases (expired link, email mismatch, sign-in cancelled/failed) share the same Giant Swarm-styled page.
- Per-thread `/details` settings, `/usage` figures, and resume-check marks are dropped after 24 hours of thread inactivity, so a long-lived gateway's memory stays bounded. An active thread refreshes its state on every turn.

### Fixed

- Slack `/usage` sent as a new top-level message in a DM now reports the conversation's token usage. Usage is keyed by the turn's thread root, and a top-level DM message keys a brand-new thread, so the command always answered "Token usage not available yet."; DM usage is now also aggregated per conversation. In a regular channel, `/usage` typed outside the agent's thread replies with guidance to run it as a reply inside the thread.
- A transient failure of the resume existence-check no longer suppresses the "starting fresh" notice for that thread forever. Only a conclusive result (session present or gone) marks the thread as checked; an indeterminate check is retried on the thread's next message.
- Slack OBO: the gateway forwards the dex id_token on the A2A request instead of muster's opaque access token. kagent's A2A edge validates the caller by decoding the JWT and reading its `sub` claim; the opaque muster token is not a JWT, so it was rejected with 401 and linked users' turns never reached the agent. The id_token is the dex-issued token muster returns alongside its access token; the access token is still used only to read the linked identity from the userinfo endpoint. A token response without an id_token now fails loudly instead of forwarding an unusable credential.
- Slack OBO: the sign-in callback now fails when muster's token response carries no id_token (missing `openid` scope or upstream IdP misconfiguration), instead of storing a link that reports "Signed in to Giant Swarm" and then errors on every turn. No link is persisted in that case.
- Slack events without a user ID are ignored; an anonymous event can no longer start a turn.
- Button-click resumes of paused tool approvals carry the clicking user's human token when linking is enabled, the same as typed replies; a resume without one is aborted instead of running as the gateway service account.
- Slack OBO: when a linked user's muster refresh token is rejected (`invalid_grant`), the gateway now drops the dead link and prompts sign-in with the Block Kit button on the same turn, instead of posting a "couldn't refresh, try again" message and only showing the button one turn later. A transient token-endpoint failure (5xx, network) still keeps the link and returns the retryable error.
- Slack human-token forwarding no longer silently falls back to the M2M service-account identity. When linking is enabled, the human's muster token is the only credential forwarded; a turn without a valid one is aborted instead of degraded — an unlinked user is prompted to sign in, and a linked user hitting a transient token-mint failure gets a clear error in-thread. This removes a confusing, privilege-broadening fallback that also masked real failures (klaus-gateway#116).
- Link-store key accepts a base64- or hex-encoded 32-byte AES-256 key, not only 32 raw bytes. A SOPS-staged 44-char base64 `obo.storeKey` previously crashed the gateway on boot (`encryption key must be 32 bytes, got 44`), forcing the in-memory store (links lost on every restart). The key is now normalized at startup; a value that does not resolve to exactly 32 bytes still fails loudly.
- Slack OBO: the gateway caches the short-lived human muster access token and serializes refreshes per Slack user instead of refreshing on every message. `TokenFor` previously ran a `refresh_token` grant on every call; because muster rotates refresh tokens and a single Slack turn drives several `TokenFor` calls (the Events API retries an event the agent takes >3s to answer, and concurrent messages overlap), the second refresh reused an already-rotated token, failed `invalid_grant`, and dropped the link. The gateway then silently fell back to the M2M service-account identity, which muster's `/mcp` edge rejected (`audience [kagent]`), leaving the agent with no muster tools. Tool calls now stay attributed to the human for the access token's full lifetime, refreshing at most once per token.
- Updated `tests/test-values.yaml`: removed stale `lifecycle.operatorMCPURL` override and bumped `image.tag` from `0.0.44` to `0.1.4`. The old pin ran the binary that rejected `--driver=static` with empty instances, causing CrashLoopBackOff.
- Slack replies larger than a single Slack message no longer fail the `chat.update` call; the batched writer rolls the overflow over into stable follow-up in-thread messages, and an in-progress (unterminated) code fence is left unformatted instead of being mangled by mrkdwn transforms. The rollover never splits a multi-byte UTF-8 rune when a single line exceeds the message limit.
- Avoid a potential panic from comparing `OutboundDelta` (which embeds an error interface) against its zero value on the A2A error path.
- Slack turns are serialized per thread. A message that arrives while the thread's previous turn is still running gets a brief "still working" notice instead of starting a second, overlapping turn; concurrent turns on one thread share a kagent session and would otherwise interleave its event log into incoherent history.
- A Slack thread's paused input-required task is no longer consumed by a typed reply that then aborts on a transient human-token error. The pending task is taken only once the turn is committed to run, so a later button click can still resume it instead of finding nothing.
- A rate-limited Slack Web API call (HTTP 429) no longer aborts the turn and discards the agent's work. The call is retried once after honoring `Retry-After`, unless the server asks to wait more than 30s (then it fails fast rather than stalling the writer), and a failed flush keeps its content pending so the next flush re-sends it instead of dropping the delta.
- Slack deliveries are deduplicated by `event_id` in the shared inbound pipeline, so a duplicate delivery never starts a second turn on either transport (Events API and Socket Mode) while a retry whose original delivery was lost (pod restart, ingress failure mid-request) is still processed. Dedup previously lived only in the Events API handler, so Socket Mode redeliveries could start a duplicate turn (klaus-gateway#131). An Events API retry without an `event_id` is still dropped at the HTTP handler.
- Slack HITL button clicks resume the paused task with the clicker's human muster token. Previously the button path skipped the on-behalf-of token resolution, so an approved mutating tool call executed as the gateway service account instead of the approver; an unlinked clicker is now prompted to sign in and the pending prompt stays answerable.
- Agent replies chunked into Block Kit `markdown` blocks can no longer exceed Slack's 12,000-character block limit when a chunk boundary falls inside a fenced code block with a long info string; the fence close and reopened fence line are budgeted inside the cap, so the flush is never rejected.
- Leading Slack link and channel tokens (`<https://...>`, `<#C...>`) in a mention are no longer stripped with the `<@USERID>` mention prefix; a message consisting of a mention plus a URL reaches the agent instead of being dropped as empty.
- Token usage is no longer over-counted when the model provider reports usage on streaming chunks: usage deltas skip partial events, which mirror the same LLM call's counts on every chunk.
- Token usage reported on a failed, rejected, or canceled terminal event is now carried on the error delta and counted before the turn aborts, instead of being discarded so a failed turn's tokens never reached usage accounting.
- Agent-rendered text is escaped before entering Slack mrkdwn contexts (approval prompts, ask_user questions and choices, decision labels), so quoted content containing `<!channel>`, `<!here>`, or `<@U...>` renders literally instead of triggering notifications.
- `users.info` lookups go through the same 429-retrying Slack transport as every other Web API call.
- A non-2xx Slack Web API response (other than 429) surfaces as an error carrying the HTTP status code instead of a JSON decode failure on a non-API body.
- The notification fallback text of agent replies is escaped, so agent output containing `<!channel>`, `<!here>`, or `<@U...>` can no longer trigger notifications through the fallback while the rendered blocks stay literal.
- HITL approval and choice prompts truncate their text to Slack's 3000-character section limit (rune-safe, with a trailing ellipsis), so an oversized prompt posts instead of being rejected with `invalid_blocks` and stranding the paused task.
- A transient failure removing the working progress reaction no longer strands it; the removal is retried on the next progress update.
- A fence info string too long for the chunk budget degrades the continuation to a plain split instead of emitting a block over Slack's limit, and a fence-open line longer than the budget still toggles fence state, so prose after the fence is no longer wrapped in reopened code fences.
- A Slack turn that ends in error while in text-progress mode replaces its `_thinking…_` placeholder with a failure note instead of leaving it dangling with no signal (reactions mode already swaps in the failed emoji). An intentional `/stop` still cancels silently.

### Refactored

- Factor the duplicated Slack inbound pipeline (Events API + Socket Mode) into a single `Adapter.handleInbound` so both transports behave identically.

### Changed

- Slack shows turn progress with message reactions on the triggering message (a working emoji added while the turn runs) instead of a `_thinking…_` placeholder. On success the working reaction is removed with no residual emoji by default; set `SLACK_CLEAR_REACTION_ON_DONE=false` to swap in a done reaction instead. A failed turn swaps in the failed reaction. `SLACK_PROGRESS_MODE` selects `auto` (default), `reactions`, or `text`; `auto` falls back to a text placeholder when the `reactions:write` scope is unavailable. Requires the new `reactions:write` bot scope.
- Slack agent replies render as Block Kit `markdown` blocks (Slack's native Markdown: bold, italic, lists, tables, code blocks) instead of the lossy mrkdwn conversion, chunked at 12,000 characters with code-fence-aware rollover into follow-up in-thread messages.
- `--driver=static` no longer requires `--static-instances` to be non-empty. An empty static instance set is valid and acts as a no-op lifecycle manager, allowing A2A-only deployments (Slack/CLI/web → kagent) without any Klaus instance management.
- `a2a.A2AClient.TokenPath` (string) is replaced by `a2a.A2AClient.TokenSource` (the `a2a.TokenSource` interface). `a2a.FileTokenSource` reproduces the previous per-request file read; `a2a.ForwardedTokenSource` prefers a caller token from the request context and falls back to a `TokenSource`.
- `a2a.ForwardedTokenSource` gains `ForwardedOnlyChannels`: requests from a listed channel never use the fallback token source, a missing forwarded token is an error. The originating channel travels on the egress context via `a2a.WithChannel` / `a2a.ChannelFromContext`. With OBO linking enabled the gateway lists the Slack channel, so a Slack turn can never fall back to the gateway ServiceAccount identity; the web and cli channels keep the fallback for anonymous callers.

### Added

- `SLACK_WORKING_EMOJI` / `SLACK_DONE_EMOJI` / `SLACK_FAILED_EMOJI` (and the matching `--slack-working-emoji` etc. flags) override the progress reaction emoji names; empty uses `eyes` / `white_check_mark` / `x`.
- kagent token usage (`kagent_usage_metadata`) and tool-call activity (`kagent_type` function_call/response DataParts) are parsed from A2A events into `OutboundDelta`: a new `Usage *TurnUsage`, a typed `Tool *ToolActivity` (name/args/response, provider-neutral), and a `DeltaToolActivity` kind. `mapA2AEvent` now returns `[]OutboundDelta` (an event may carry both text and tool activity).
- Helm: `obo.persistence` backs the link store with a PersistentVolumeClaim so links survive pod recreation (rollouts, node drains), not just in-process restarts. Disabled by default (emptyDir, unchanged behaviour). When enabled with a single ReadWriteOnce claim the Deployment switches to the `Recreate` strategy and sets `fsGroup` so the non-root runtime user can write the encrypted bolt file. `obo.persistence.existingClaim` reuses a pre-provisioned PVC.
- Helm: `slack.dmOnly` value renders the `SLACK_DM_ONLY` env, restricting the Slack adapter to direct messages (channel messages and @-mentions are ignored). The binary already honoured `SLACK_DM_ONLY`; this exposes it through the chart.
- Helm: `slack.botToken` / `slack.signingSecret` / `slack.appToken` values render the `slack.secretName` Secret (keys `bot-token` / `signing-secret` / `app-token`) when supplied — typically SOPS-encrypted via the agentic-platform umbrella. Mirrors the existing `obo` secret rendering. When `botToken` is empty the chart still references an externally-managed Secret of that name, so existing deployments are unaffected.
- A2A egress forwards the caller's inbound bearer token: `InboundMessage.BearerToken` is captured from the `Authorization` header by the web and CLI adapters and sent as `Authorization` on the A2A request, so kagent (trusted-proxy) sees the end-user identity. Channels with no per-user token (Slack) fall back to the `--a2a-token-path` ServiceAccount token. New API in `pkg/a2a`: `TokenSource`, `FileTokenSource`, `ForwardedTokenSource`, `WithForwardedToken`, `ForwardedTokenFromContext`.
- `channels.SynthesizeContextID` derives a stable A2A contextID from `(channel, channelID, userID, threadID, agentRef)` using length-prefixed SHA-256 encoding.
- `slack.defaultAgent` config (`--slack-default-agent` / `KLAUS_GATEWAY_SLACK_DEFAULT_AGENT`): every Slack thread routes to this named agent. Required when Slack is enabled; validated against the static instance set at startup when `--driver=static`.
- Channel turns are now routed through the A2A executor (`ForwardingExecutor.Execute`) when `Facade.Executor` is set and `InboundMessage.AgentRef` is non-empty. Artifact events map to `OutboundDelta{Content}` and terminal status events map to `OutboundDelta{Done: true}`.

### Changed

- `Facade.SendCompletion` dispatches through A2A when the inbound message carries a non-empty `AgentRef` and `Facade.Executor` is set. Requests with no `AgentRef` (web and CLI channels) continue to use the OpenAI `/v1` SSE path unchanged.

### Changed

- Bump `giantswarm/architect` orb to `8.2.2` and re-enable cosign keyless chart signing (`sign: false` removed from every `push-to-app-catalog*` invocation). v8.2.2 ships [architect-orb#772](https://github.com/giantswarm/architect-orb/pull/772) which upgrades the `app-build-suite` executor image from `1.8.0-circleci` to `1.8.1-circleci` -- the new image includes the `cosign` binary that v8.2.0's chart signing defaults require. Closes [architect-orb#769](https://github.com/giantswarm/architect-orb/issues/769).
- Disable cosign keyless chart signing on the `push-to-app-catalog*` jobs (`sign: false`). The architect orb's `push-to-app-catalog` defaults `sign` to `true` since v8.2.0 and shells out to `cosign`, but this repo uses `executor: app-build-suite` (so the `app_build_suite` Python CLI is available to package the chart with metadata) and the `app-build-suite` image doesn't ship `cosign`. Without this opt-out, every chart push fails on the `Mint Sigstore OIDC token` step with `cosign: command not found`. To be removed once architect-orb makes `cosign-prepare` resilient to a missing binary (or ships cosign in the `app-build-suite` executor).
- Bump `giantswarm/architect` orb to `8.2.1` to pick up [architect-orb#767](https://github.com/giantswarm/architect-orb/pull/767): `image-login-to-registries` is now POSIX-portable, unblocking `architect/sync-china-registry` (the gsoci -> Aliyun mirror via the in-China `giantswarm/galaxy-runner`). The v8.1.0 refactor accidentally introduced bash-only `${!var}` indirect expansion in the shared login command, which BusyBox `/bin/sh` (used by the regctl executor) rejected with `bad substitution` -- so no Aliyun mirror has been happening since the migration to `split-china-push: true`. v8.2.x also enables cosign keyless signing, SLSA provenance, and SBOM attestations by default for public images and charts.
- Replace the `push-to-gsoci-release` + `push-to-all-registries-release` workaround pair (gsoci-only push gating the chart, plus a parallel best-effort all-registries push to dodge Aliyun timeouts) with a single `push-to-registries-release` job using `split-china-push: true` and a companion `sync-china-registry` job. The cross-Pacific `docker buildx` push to the Aliyun mirror is gone; the in-China `giantswarm/galaxy-runner` runs `regctl image copy` from gsoci to Aliyun via the Singapore geo-replica. The chart catalog publish still does not gate on Aliyun.
- Bump `giantswarm/architect` orb to `8.1.0` and migrate image pushes from the deprecated `push-to-registries-multiarch` job to `push-to-registries` with `multiarch: true`. Picks up the v8.1.0 QEMU/binfmt auto-registration, hardened buildx bootstrap, and standard OCI image labels.

### Fixed

- Pin container name in the Deployment template to the literal `klaus-gateway` rather than
  `{{ .Chart.Name }}`. When the chart is consumed as a Helm dependency under a camelCase
  alias (e.g. `alias: klausGateway`), Helm sets `.Chart.Name` to the alias, producing an
  RFC 1123-invalid container name that prevents the Deployment from being applied.

- Add `.abs/main.yaml` with `replace-chart-version-with-git` /
  `replace-app-version-with-git` enabled. Without this config app-build-suite
  packaged the chart with the literal `0.1.0` placeholder from `Chart.yaml`,
  which left the published chart's `appVersion` (and thus the default
  `image.tag`) pointing at the non-existent `:0.1.0` image. The same flag is
  used by `klaus` and `mcp-prometheus`.

### Changed

- Switch the chart catalog jobs to the `app-build-suite` executor (mirrors the
  klaus and mcp-prometheus pattern). `app-build-suite` rewrites `Chart.yaml`'s
  `version` and `appVersion` from the git tag at build time, which finally
  lets tag releases publish a chart -- previously every tag build failed
  architect's strict `helm-chart-template` validator because
  `pkg/project/project.go` keeps the literal value `dev`.
- Hardcode `version`/`appVersion` placeholders in `helm/klaus-gateway/Chart.yaml`
  back to `0.1.0`. The CI's `app-build-suite` step overwrites them; templating
  via `[[ .Version ]]` (introduced in #19) is incompatible with that flow.
- Split the tag-build registry push into two parallel jobs: a gsoci-only push
  that gates the chart catalog release, and a separate "all registries" push
  that also covers the slow China mirror. The chart push no longer waits for
  the China mirror, so a slow mirror only delays itself.

[Unreleased]: https://github.com/giantswarm/REPOSITORY_NAME/tree/main
