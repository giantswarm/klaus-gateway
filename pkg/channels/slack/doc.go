// Package slack is the Slack channel adapter for klaus-gateway.
//
// Two connection modes are supported:
//   - events: Slack Events API HTTP webhook (production).
//   - socketmode: Slack Socket Mode WebSocket (development).
//
// The adapter is disabled by default; set --slack-enabled (or
// KLAUS_GATEWAY_SLACK_ENABLED=true) to activate it.
//
// # Vocabulary
//
// The operator-facing companion to these terms lives in
// docs/channels-slack.md; identifiers across the package rely on them.
//
//   - thread: one Slack thread (threadID is the root message's ts; in a DM,
//     the message's own ts). A thread maps 1:1 to one kagent session, which
//     is why turns on a thread are serialized.
//   - session: the kagent session backing a thread. It is keyed by the
//     synthesized context ID and the principal the forwarded token resolves
//     to, which is why every turn on a shared thread must forward the same
//     identity.
//   - turn: one inbound trigger (a typed message or a button click) and the
//     completion streamed back for it. At most one turn runs per thread at a
//     time (the thread's turn slot, threadState in thread.go); a concurrent
//     trigger is rejected busy.
//   - task: an A2A task. A turn that pauses at input-required leaves a
//     pending task on the thread; a later turn (a typed reply or a button
//     click) resumes it. A task can span several turns; a turn serves at
//     most one task. Turn and task are distinct on purpose: renaming one
//     into the other would erase the pause/resume relationship.
//   - initiator: the first user to interact in a thread. Collaborators need
//     the initiator's consent, and every turn on the thread is forwarded
//     under the initiator's identity, so the shared session sees a single
//     principal.
//   - collaborator: a user the initiator granted. Their turns are attributed
//     to them (msg.Author) but run under the initiator's token.
//
// # Turn flow
//
// Two entrypoints admit turns, each with its own gates, and converge on the
// shared tail, runTurn (turn.go):
//
//   - dispatch (slack.go), a typed message: access gate (initiator or
//     granted collaborator; newcomers park for consent), sign-in gate (a
//     signed-out sender's message parks for replay after linking), thread
//     slot, then take the thread's pending task, mapping the reply text to
//     a HITL decision.
//   - handleDecision (interactions.go), a Block Kit click: permission gate,
//     thread slot, pending-task peek with stale-prompt and completeness
//     gates, sign-in gate (prompt only: a click has no message to park),
//     then take the task the gates validated against.
//
// runTurn is everything that runs the same way regardless of entrypoint:
// resolve the sender's email, apply the initiator's identity, resolve the
// agent, register the turn for /stop, send, and stream. It owns the
// restore-on-failure guard for the taken task and corrupt-session recovery.
package slack
