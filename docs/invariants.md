# Invariants

Facts about the systems around this gateway that a change must respect. Each one cost a
review round or a live failure before it was written down; the pull request in brackets is
the evidence. Read this before changing `pkg/channels/slack`, `pkg/channels`, `pkg/routing`
or `helm/`. Add a line when a review finds a new one.

## Slack

- **Nothing a person waits on lives only in memory.** A message parked for a sign-in or an
  approval and a paused prompt are written to the thread's row (`Entry.Held`) on every
  change and read back at start; the sign-in's PKCE verifier is derived from its signed
  state. A new piece of per-thread state a restart must not lose goes there too (#132).
- **A mention inside a thread arrives twice**, as `app_mention` and as `message.channels`.
  A gate on the plain-message copy must skip text that mentions the bot, or it answers the
  mention with a request to mention (#307).
- **Slack acknowledges an interaction before the gateway acts on it.** The 200 goes out,
  then the handler runs in the background. A test that sends the next message on the ack
  races the write; wait for the write's own side effect, such as the `response_url` rewrite
  that follows a grant (#287).
- **A thread's conversation is its initiator's.** Its Session is created under the
  initiator's identity; a granted collaborator's turns run under their own token and reach
  that session through the thread's Session share (#350). Letting a person in is the
  initiator's consent decision, taken through the Allow prompt; no entry point grants a
  newcomer as a side effect (#279).
- **kagent answers NotFound to anyone who is not the session's creator.** A lookup, delete
  or share revoke under a collaborator's token without a share gets NotFound for a session
  that exists. Never read that as "gone": do not clear the binding, report a reset or log a
  revoke on it. Only the creator's token, or a share, makes the answer mean something (#356).
- **kagent 1.2.2 and later report token usage only on the artifact.** `kagent.dev/a2a/usage`
  rides on an artifact update's artifact metadata, once per LLM call; status updates and the
  task's own metadata do not carry it. Read it there, or every turn counts 0 tokens and `usage`
  has nothing to show (#397). The artifacts of a whole Task carry it too (a2a-go keeps artifact
  metadata): from a whole Task count only the artifacts whose usage the stream did not deliver,
  by artifact ID (#401).
- **A turn that an approval resumes streams the approved call's result, not the call.** The
  call arrived in the turn that asked for approval. The writer of the resumed turn takes it
  from the pending task (`approvedCalls`, replayed at the start of `run()`), and that replay
  is what puts the call, and the tool `call_tool` really ran, in the tool log. Without it the
  result reads "`call_tool` result" (#370).
- **Payload fields change shape between message kinds.** A block element's `text` is a
  string in rich text and a `{type, text}` object in a button; PagerDuty posts carry both.
  Decode leniently: accept both shapes, and a type mismatch in one field skips that field,
  never the whole read (#288).
- **A `response_url` ephemeral appears in the channel view**, not in the thread pane. A
  tester who looks at the thread sees nothing (#288 live test).
- **A `response_url` replacement of an ephemeral stays where that ephemeral is.** With the
  thread's `thread_ts` and `replace_original: true`, a sign-in card in a thread became the
  confirmation in the thread. The Sign in click reaches the gateway seconds before the link
  completes, so its `response_url` is there in time (#346 live test).
- **Slack applies a new OAuth scope only on re-install** of the app. A manifest change alone
  changes nothing on an existing install (#288, `UPGRADE.md`).
- **A mention notifies only when the message is posted.** A mention that `chat.update` adds
  to a message sends no notification, so a notice that names a second person does not ping
  them (#332).
- **The composer keeps a message that starts with `/` for Slack's own commands**, in a DM
  too, so such a message reaches the bot only after a mention. The gateway's own commands are
  plain words for that reason: a message that
  is exactly `usage`, `help`, `agents`, `login` or `logout` runs that command wherever the bot reads,
  `mute` does the same in a channel thread, and `stop` in a thread with a running turn. No in-message slash command is
  served any more, so a message that starts with one goes to the agent. The Slack API sends
  `/stop` as text, so a test through the API does not show this (#339 live test).
- **A suggested prompt that starts with `/` never reaches the bot.** Slack runs its text as
  a Slack command, the same as the composer, and it removes a leading space first, so
  `" /agent"` fails too. A prompt whose message is exactly one command word does run that
  command, because Slack sends it as an ordinary message (#344 live test on glean).
- **A `response_url` from a button on a normal message replaces that message.** A refusal
  sent through it overwrote the public roster for everyone. Answer such a click with a
  thread-scoped ephemeral, and send `"replace_original": false` on every `response_url` reply
  that is meant as a new message (#343 live test).
- **A streamed message that holds text is refused near 13,800.** Slack answers an append or a
  stop with `msg_too_long` once the message outgrows about 13,800 characters of text, so the
  12,000-character cap leaves a margin. A `task_update` step would count toward the same limit
  as a card (about 90, plus 160 per field, plus its JSON-escaped characters), far more than its
  characters, and an update's `output` and `details` add to the card rather than replace it
  (#358, replays on graveler, 2026-09-28).
- **Every message in a served channel reaches the inactive-thread gate.** That path is the
  most frequent one the gateway runs; it costs at most one store read (#307). A reply under the
  bot's own message (`parent_user_id`) costs one more: the lookup of a decision it may answer or a
  conversation it belongs to, one record under one id (#360, #369).

## Thread state and the store

- **The routing store is the single source of truth for thread state.** Slack history is
  never read to recover an agent, an initiator or a grant (#272). Reading a thread to hand
  its messages to the agent is a different purpose and is documented as such (#288).
- **Valkey expires and evicts silently.** The gateway acts before a row's expiry, never on
  it; a row that must outlive its conversation carries its own longer TTL (#307).
- **The Session request id is `SynthesizeContextID(channel, channelID, "", threadID, agentRef, workspace)`**
  with an empty user slot, and the workspace hashed only when one is chosen. It is the
  controller's idempotency key: change it, and every live thread asks for a session the
  controller does not hold (#320, #428).
- **An A2A call is routed by its tenant and the message's context id.** The tenant is the
  Agent, `namespace/name`; the context id is the Session's id, which the controller reports as
  its `context_id`. There is no routing header, and a call whose context id names another
  Session is refused.
- **A thread's mute is the row's `muted_at`, its own field, not part of `held`.** The
  inactive-thread gate reads it from the one row it already reads, so the gate path still costs
  one store read; `held` is rewritten from the adapter's memory on every change of a parked
  queue, and a mute kept there would be lost on the next one (#410).
- **The controller's create is idempotent per caller and request id.** The same person gets
  the earlier session back; a different person gets a new one (giantswarm#37896).

## Tests

- **Flow tests wait with `flowWait`, `waitThreadIdle` and `waitTurnStreaming`**, never with a
  literal duration or a `time.Sleep` (#285, #287).
- **A step that posts twice is asserted on its second post.** The consent prompt to the owner
  goes out before the newcomer's ack; asserting on the first ephemeral flakes (#318).
- **The Slack package passes `go test -race -count=3`.** A failure only under `-count>1` is
  state shared between tests, not slowness (#285).

## Chart and platform

- **agent-platform forwards its whole `klausGateway` block verbatim, and this chart's schema
  refuses unknown keys.** Deleting or renaming a values key is a major release, and the meta
  chart must admit that major first (#271, #497, #319). The layers are described in
  `docs/deployment.md`, "Where an installation's values come from".
- **A key the chart stops reading stays in `values.yaml` as a documented no-op** until the
  umbrella and the config layers stop sending it; only then does the schema drop it (#320,
  #636, shared-configs#743, #328).
- **The customer BOM in agent-platform pins a component version.** A change to what the
  chart needs from its values must move that pin (#636).
- **agent-platform's golden render compares against current `main`.** Merge `main` right
  before pushing there, and drop a golden-only hold in the same change that lands its
  removal on `main`, or every branch fails the check (#643).
