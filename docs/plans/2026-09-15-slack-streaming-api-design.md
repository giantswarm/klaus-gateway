# Slack streaming API for agent replies

Date: 2026-09-15. Status: validated design, implementation in two PRs.

## Goal

Replace the Slack adapter's post-and-edit rendering of agent replies
(`batchedWriter` in `pkg/channels/slack/stream.go`: `chat.postMessage` on the
first flush, `chat.update` every 250 ms, rollover into follow-up messages at
12,000 characters) with Slack's streaming API: `chat.startStream`,
`chat.appendStream`, `chat.stopStream`.

## Why

- `chat.update` is documented at one call every 3 seconds per message. The
  adapter edits far more often and lives on the retry after a 429.
  `chat.appendStream` is tier 4 (100+ calls/min for the app) and each call
  carries only the new text, not the whole re-rendered message.
- Slack renders a streamed message with its own progressive animation.
- The Stop button halts the stream on Slack's side; the app learns it through
  `stopped_by_user`, which removes the race between the cancel and the last edit.
- `chat.stopStream` accepts `session_status`, so the exit status of the agent
  session travels with the final text.
- A stream creates the agent session; unfurls are off on streamed messages.

## Slack API facts (docs.slack.dev, verified 2026-09-14/15)

- `chat.startStream`: bot token, `chat:write`. Args: `channel`, `thread_ts`
  (omit only for session channels, which we do not use), `markdown_text`
  (<= 12,000 chars) or `chunks`, `recipient_user_id` + `recipient_team_id`
  (mandatory when streaming in a channel), `icon_emoji`/`icon_url`/`username`
  (display identity, needs `chat:write.customize`), `task_display_mode`.
  Returns `channel`, `ts`. Tier 2 (20+/min).
- `chat.appendStream`: `channel`, `ts`, `markdown_text` or `chunks` (never
  both; the mode must match the start call). Tier 4 (100+/min). Errors:
  `message_not_in_streaming_state`, `stopped_by_user`, `streaming_mode_mismatch`.
- `chat.stopStream`: `channel`, `ts`, optional `markdown_text`/`chunks`,
  `blocks` (<= 50, rendered at the bottom), `metadata`, `session_status`
  (`active` | `processing` | `suspended` | `closed`, DEFAULT `active`).
  Tier 2 (20+/min). Errors: `message_not_in_streaming_state`,
  `message_not_owned_by_app`.
- Blocks are allowed on `chat.stopStream` only, not on start or append.
- `agent_session_stopped` carries `streaming_message_ts`: the streams Slack
  halted when the user pressed Stop.
- No beta, plan or Marketplace gate on any of the three methods.

## Decisions

1. Two PRs. PR 1 streams the reply text and leaves the tool ticker, the
   receipt and the prompt handling untouched. PR 2 moves the tool steps into
   `task_update` chunks on the same stream and removes the ticker/receipt.
2. No feature flag. The post-and-edit path is deleted in PR 1. Rollback is the
   previous image.
3. Flush cadence: the batching tick stays as a mechanism, its interval becomes
   1 s (`streamAppendInterval`). The first text delta is flushed at once so the
   message appears immediately; the tick governs only the appends after it.
   Rationale: 60 appends/min per active thread fits tier 4 with headroom for a
   second thread; the existing 429 handling with `Retry-After` is the safety
   net. A shared pacer across writers is a follow-up only if 429s appear.
4. Tier 2 on start/stop bounds the app to ~20 turn starts per minute across
   the workspace. Acceptable today; expose a metric.

## PR 1 design

### Client (`slackAPIClient`)

- `startStream(ctx, channel, threadTS, markdown, recipientUser, recipientTeam string) (ts string, err error)`
- `appendStream(ctx, channel, ts, markdown string) error`
- `stopStream(ctx, channel, ts, markdown string, status sessionStatus) error`
  (`status` is always sent explicitly; never rely on the `active` default).
- JSON bodies via the existing `postJSON`/`call` plumbing; typed error codes via
  `hasErrorCode`. New sentinel errors: `errStreamStoppedByUser`,
  `errStreamNotStreaming`, `errStreamNotOwned`.
- Display identity (`username`, `icon_url`) applies to `chat.startStream` the
  same way it applies to `chat.postMessage` today, including the unbranded
  retry when the customize scope is missing.
- `recipient_user_id` / `recipient_team_id` are sent only when the channel is
  not a DM (`isDMChannelID`). The user is the Slack user of the turn (new
  writer field set in `streamResponse`). The team is the bot's team from the
  cached `auth.test` identity. Slack Connect channels are out of scope
  (comment).

### Writer (`batchedWriter`)

- Remove: head/tail message timestamps, `postMarkdown`, `chatUpdateMarkdown`,
  the follow-up-message rollover, `batchInterval` (250 ms),
  `slackMarkdownBlockMax` renamed/reused as the per-stream text cap (12,000).
- Add: `streamTS string`, `streamed int` (chars sent on the current stream),
  `pending` (unsent text), `streamRecovered bool`, `slackUser string`.
- `flush`: send the pending text up to the last whitespace boundary (hold back
  the tail so the login-URL scrubbing never sees half a URL); if no stream is
  open, `startStream`, else `appendStream`. If `streamed + len(chunk)` would
  exceed 12,000, `stopStream(current, "", sessionProcessing)` then
  `startStream` with the chunk (rollover). The turn is still running, so the
  intermediate stop MUST carry `processing`, never the default `active`.
- `finalFlush`: `stopStream(streamTS, remaining text incl. held-back tail,
  exitSessionStatus())`. When a stream was opened, the separate exit
  `setSessionStatus` call is skipped; when no stream was opened in the turn
  (prompt-only turn, no text), the existing exit call runs unchanged. If the
  final stop fails for a reason other than `stopped_by_user`, fall back to the
  existing exit `setSessionStatus` (with its retry) so the indicator cannot
  stick.
- The `processing` status call at turn start (with the title) stays as is.
- `stopped_by_user` on append or stop: treat as a clean end of the text path:
  no retry, no error notice, no further calls on that `ts`. The stop-button
  handler keeps posting the "Stopped by @user" notice as a separate message.
  Decode `streaming_message_ts` in the event; log it at debug.
- `/stop` command and agent errors: the run is cancelled/errored; the writer
  closes the stream with `stopStream` on that exit (status `active`), and the
  error notice stays a separate message as today.
- `message_not_in_streaming_state` on append: open a new stream for the
  remaining text (same move as rollover), once per turn; a second occurrence
  drops the text path for the turn with a warning.
- Retract of the streamed bubble (`retractRendered`, connector sign-in prompt
  replacing it): `stopStream(ts, "", sessionProcessing)` first, then
  `chat.delete`. The retract condition itself does not change.
- Ordering with the thread poster (tool ticker/receipt posts) is unchanged;
  the final stop happens in `finalFlush` as the update did.

### Tests

- Fake Slack server (internal `stream_internal_test.go` fake thread and the
  external `slack_test.go` fake): implement the three methods with real state:
  streaming after start; append/stop on a non-streaming ts return
  `message_not_in_streaming_state`; a test hook flips a stream to
  `stopped_by_user`. Record every call with its text.
- Writer tests: first delta starts at once with identity; later deltas append
  increments only; tail hold-back; DM vs channel recipient IDs; rollover stops
  with `processing` and starts a new stream; final stop carries `active` or
  `suspended`; stopped-by-user ends quietly; one recovery then warning;
  prompt-only turn uses the exit status call; error turn stops the stream and
  posts the notice; retract stops before delete; final stop failure falls back
  to the exit status call.
- Flow tests: one DM and one channel end-to-end asserting the sequence
  start, append, stop on the fake's recorded paths.
- Metrics (Prometheus, `pkg/observability`): streams started, stopped,
  stopped by user, recovered.

### Docs

- `docs/channels-slack.md`: describe the streaming path, the 1 s cadence, the
  12,000-char rollover, the Stop interplay.
- `CHANGELOG.md`: one bullet under Unreleased, consumer effect only.
- This design under `docs/plans/2026-09-15-slack-streaming-api-design.md`.

## PR 2 outline (after PR 1 review)

- Tool ticker and receipt become `task_update` chunks (`id`, `title`,
  `status` in_progress/complete/error, optional `details`) on the same stream;
  `task_display_mode` timeline (default).
- `/details` levels: off = no task chunks; on = titles only; full = titles with
  details/output.
- Tool names in plain language (strip `x_`/`workflow_` prefixes, humanise).
- Remove the in-thread status message posts/edits for tool activity.

## Manual test plan (graveler)

DM and channel thread, each: short answer; answer over 12,000 chars; Stop
button mid-answer; `/stop` mid-answer; HITL `ask_user` question; connector
sign-in prompt. Watch 429s and the new stream metrics.
