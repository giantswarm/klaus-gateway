# Invariants

Facts about the systems around this gateway that a change must respect. Each one cost a
review round or a live failure before it was written down; the pull request in brackets is
the evidence. Read this before changing `pkg/channels/slack`, `pkg/channels`, `pkg/routing`
or `helm/`. Add a line when a review finds a new one.

## Slack

- **A mention inside a thread arrives twice**, as `app_mention` and as `message.channels`.
  A gate on the plain-message copy must skip text that mentions the bot, or it answers the
  mention with a request to mention (#307).
- **Slack acknowledges an interaction before the gateway acts on it.** The 200 goes out,
  then the handler runs in the background. A test that sends the next message on the ack
  races the write; wait for the write's own side effect, such as the `response_url` rewrite
  that follows a grant (#287).
- **A thread acts under its initiator's delegated identity.** Letting a person in is the
  initiator's consent decision, taken through the Allow prompt; no entry point grants a
  newcomer as a side effect (#279).
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
  too, so a plain `/stop` never reaches the bot. A command needs a mention first
  (`@bot /stop`); in a thread with a running turn a plain `stop` works. The Slack API sends
  `/stop` as text, so a test through the API does not show this (#339 live test).
- **A suggested prompt that starts with `/` never reaches the bot.** Slack runs its text as
  a Slack command, the same as the composer, and it removes a leading space first, so
  `" /agent"` fails too. A prompt cannot run a gateway command (#344 live test on glean).
- **A `response_url` from a button on a normal message replaces that message.** A refusal
  sent through it overwrote the public roster for everyone. Answer such a click with a
  thread-scoped ephemeral, and send `"replace_original": false` on every `response_url` reply
  that is meant as a new message (#343 live test).
- **Every message in a served channel reaches the inactive-thread gate.** That path is the
  most frequent one the gateway runs; it costs at most one store read (#307).

## Thread state and the store

- **The routing store is the single source of truth for thread state.** Slack history is
  never read to recover an agent, an initiator or a grant (#272). Reading a thread to hand
  its messages to the agent is a different purpose and is documented as such (#288).
- **Valkey expires and evicts silently.** The gateway acts before a row's expiry, never on
  it; a row that must outlive its conversation carries its own longer TTL (#307).
- **The AgentInstance request id is `SynthesizeContextID(channel, channelID, "", threadID, agentRef)`**
  with an empty user slot. It is the controller's idempotency key: change it, and every live
  thread asks for an instance the controller does not hold (#320).
- **The controller's create is idempotent per caller and request id.** The same person gets
  the earlier instance back; a different person gets a new one (giantswarm#37896).

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
