package slack

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// kubernetesGetResult is a tool result shaped like the x_kubernetes_get
// answers of the turn Slack refused on gazelle (2026-09-28): an MCP envelope
// around a Kubernetes object, whose preview fills a step's output with quotes,
// underscores and dots.
func kubernetesGetResult(name string) map[string]any {
	obj := map[string]any{
		"_meta": map[string]any{"effectiveNamespace": "org-example", "requestedNamespace": "org-example", "resourceScope": "namespaced"},
		"resource": map[string]any{
			"apiVersion": "infrastructure.cluster.x-k8s.io/v1beta2",
			"kind":       "AWSCluster",
			"metadata": map[string]any{
				"name": name,
				"annotations": map[string]any{
					"aws.cluster.x-k8s.io/external_resource_gc": "true",
					"giantswarm.io/last_known_cluster_upgrade":  "2026-09-14T07:49:36Z",
				},
				"finalizers": []any{"awscluster.infrastructure.cluster.x-k8s.io"},
			},
		},
	}
	doc, _ := json.Marshal(obj)
	env, _ := json.Marshal(map[string]any{"isError": false, "content": []any{map[string]any{"type": "text", "text": string(doc)}}})
	return map[string]any{"output": string(env)}
}

// answerMarkdown is an answer of about 2 700 characters with the formatting
// an agent's summary carries: bold, code spans, a table, a list.
func answerMarkdown() string {
	var b strings.Builder
	b.WriteString("Two clusters are stuck deleting, both in `org-example`. **mc-three** and **mc-four** have none.\n\n")
	b.WriteString("| MC | Cluster | Deletion started | Blocked on |\n|---|---|---|---|\n")
	for i := range 4 {
		fmt.Fprintf(&b, "| **mc-%d** | `cl-%d` | 2026-09-2%d 08:50Z | Deleting the VPC `vpc-000000000000000%d` |\n", i, i, i, i)
	}
	b.WriteString("\n## What's happening\n")
	for i := range 12 {
		fmt.Fprintf(&b, "- **Step %d:** the controller retries `ec2:DeleteVpc` and the policy denies it, so the `AWSCluster` stays at `DeletingFailed`.\n", i)
	}
	return b.String()
}

// stuckDeletingTurn is the shape of the refused gazelle turn: seventeen tool
// calls in parallel groups with object-sized results, a narration, and a
// formatted answer.
func stuckDeletingTurn() []channels.OutboundDelta { return groupedTurn(4, 4, 4, 1, 4) }

// groupedTurn is a turn of parallel tool-call groups of the given sizes, a
// narration after the second group, and the formatted answer.
func groupedTurn(sizes ...int) []channels.OutboundDelta {
	var deltas []channels.OutboundDelta
	n := 0
	group := func(size int) {
		start := n
		for range size {
			n++
			deltas = append(deltas, toolCallDeltaWith("x_kubernetes_get", fmt.Sprintf("call-%d", n), map[string]any{
				"apiGroup": "infrastructure.cluster.x-k8s.io", "management_cluster": fmt.Sprintf("mc-%d-mcp-kubernetes", n),
				"name": fmt.Sprintf("cl-%d", n), "namespace": "org-example", "resourceType": "awsclusters",
			}))
		}
		for i := start + 1; i <= n; i++ {
			deltas = append(deltas, toolResultDelta("x_kubernetes_get", fmt.Sprintf("call-%d", i), kubernetesGetResult(fmt.Sprintf("cl-%d", i))))
		}
	}
	for g, size := range sizes {
		group(size)
		if g == 1 {
			deltas = append(deltas, narrationDelta("Two clusters are in `Deleting`. Next I'll check how long they've been stuck."))
		}
	}
	answer := answerMarkdown()
	for len(answer) > 0 {
		cut := min(len(answer), 200)
		deltas = append(deltas, channels.OutboundDelta{Kind: channels.DeltaText, Content: answer[:cut]})
		answer = answer[cut:]
	}
	return deltas
}

// streamTSs returns the streamed messages the writer opened, in order.
func (f *fakeThread) streamTSs() []string {
	var out []string
	for _, c := range f.streams() {
		if c.method == methodChatStartStream {
			out = append(out, c.ts)
		}
	}
	return out
}

// requireEveryStepClosed asserts that no step card on any streamed message is
// left running, and that every step's result reached a card.
func requireEveryStepClosed(t *testing.T, ft *fakeThread, steps int) {
	t.Helper()
	withOutput := map[string]bool{}
	for _, ts := range ft.streamTSs() {
		for id, c := range ft.cardsOf(ts) {
			require.NotEqual(t, stepInProgress, c.status, "step %s on %s is left spinning", id, ts)
			if c.output != "" {
				withOutput[id] = true
			}
		}
	}
	require.Len(t, withOutput, steps, "every step's result reached a card")
}

// A turn with many large steps and a formatted answer outgrows one streamed
// message long before its text reaches 12 000 characters: Slack counts the
// steps' cards too. The steps are priced into the message's budget, so the
// reply rolls over before Slack refuses it — the turn Slack answered with
// msg_too_long on gazelle (2026-09-28) now lands whole.
func TestStream_StepsCountTowardTheMessageSize(t *testing.T) {
	ft := &fakeThread{sizeLimit: slackMeasuredLimit}
	_, _, err := runSurfaceWriter(t, ft, "C1", stuckDeletingTurn()...)
	require.NoError(t, err)

	require.Zero(t, ft.refusedTooLong(), "the budget rolls the reply over before Slack refuses it")
	require.GreaterOrEqual(t, len(ft.streamTSs()), 2, "the reply rolled over into a further message")
	require.Contains(t, ft.streamedText(), answerMarkdown(), "the whole answer is delivered")
	requireEveryStepClosed(t, ft, 17)
}

// A group of calls the agent starts at once is priced as a whole by its first
// step: the rest of the group cannot roll the message over while that step
// runs, so the group must fit where it starts. Here the message has room for
// one step of a group of eight, which therefore starts a new message.
func TestStream_WideParallelGroupsFitWhereTheyStart(t *testing.T) {
	ft := &fakeThread{sizeLimit: slackMeasuredLimit}
	w := streamWriter(t, ft, "C1")
	preview, _ := toolResultPreview(kubernetesGetResult("cl-1"), toolResultMax)
	const group = 8

	w.queueAnswer(strings.Repeat("a", slackMarkdownBlockMax-2000) + " ")
	require.NoError(t, w.flush(t.Context()))
	for i := 1; i <= group; i++ {
		w.queueStep(taskUpdate{id: stepID(i), title: "Kubernetes get", status: stepInProgress,
			details: stepField(`x_kubernetes_get {"apiGroup": "infrastructure.cluster.x-k8s.io", "management_cluster": "mc-1-mcp-kubernetes", "name": "cl-1", "namespace": "org-example"}`)})
	}
	require.NoError(t, w.flush(t.Context()))
	for i := 1; i <= group; i++ {
		w.queueStep(taskUpdate{id: stepID(i), title: "Kubernetes get", status: stepComplete, output: stepField(preview)})
	}
	require.NoError(t, w.flush(t.Context()))

	require.Zero(t, ft.refusedTooLong(), "the group does not outgrow the message it starts on")
	tss := ft.streamTSs()
	require.Len(t, tss, 2, "the group starts a new message")
	require.Len(t, ft.cardsOf(tss[1]), group, "the whole group is on the new message")
	requireEveryStepClosed(t, ft, group)
}

// The size budget is an estimate. Should Slack still refuse a batch as too
// long, the steps it closes reach their cards without the output, the full
// message is closed, and the rest of the reply continues on a new one.
func TestStream_MsgTooLongContinuesOnANewMessage(t *testing.T) {
	ft := &fakeThread{sizeLimit: 6000} // well under the writer's budget
	_, _, err := runSurfaceWriter(t, ft, "C1", stuckDeletingTurn()...)
	require.NoError(t, err, "a refusal the reply recovers from is not a rendering failure")

	require.GreaterOrEqual(t, len(ft.streamTSs()), 2)
	require.LessOrEqual(t, ft.refusedTooLong(), 2, "the size the full message held bounds the messages after it")
	require.Contains(t, ft.streamedText(), answerMarkdown(), "the whole answer is delivered")
	for _, ts := range ft.streamTSs() {
		for id, c := range ft.cardsOf(ts) {
			require.NotEqual(t, stepInProgress, c.status, "step %s on %s is left spinning", id, ts)
		}
	}
}

// A step never rolls the message over while another step on it is running:
// the running step's close must reach the card it updates. The next step to
// open once they are closed starts the new message.
func TestStream_NoRollOverWhileAStepIsOpen(t *testing.T) {
	ft := &fakeThread{}
	w := streamWriter(t, ft, "D1")
	open := func(id string) {
		w.queueStep(taskUpdate{id: id, title: "Kubernetes get", status: stepInProgress, details: strings.Repeat("d", 200)})
	}
	closeStep := func(id string) {
		w.queueStep(taskUpdate{id: id, title: "Kubernetes get", status: stepComplete, output: strings.Repeat("o", 250)})
	}

	// Text that leaves room for one open step, not two. step-2 starts in a
	// later flush, while step-1 runs.
	w.queueAnswer(strings.Repeat("a", slackMarkdownBlockMax-1200) + " ")
	require.NoError(t, w.flush(t.Context()))
	open("step-1")
	require.NoError(t, w.flush(t.Context()))
	open("step-2")
	require.NoError(t, w.flush(t.Context()))
	closeStep("step-1")
	closeStep("step-2")
	require.NoError(t, w.flush(t.Context()))
	require.Len(t, ft.streamTSs(), 1, "no roll-over while step-1 runs")

	open("step-3")
	require.NoError(t, w.flush(t.Context()))
	tss := ft.streamTSs()
	require.Len(t, tss, 2, "the next step opens the new message")
	for _, id := range []string{"step-1", "step-2"} {
		c := ft.cardsOf(tss[0])[id]
		require.NotNil(t, c, "%s stays on the first message", id)
		require.Equal(t, stepComplete, c.status)
	}
	require.Contains(t, ft.cardsOf(tss[1]), "step-3")
}

// Prose that arrives while a step runs does not roll the message over either:
// the step's close would land on a new message and leave its card spinning.
func TestStream_TextDoesNotRollOverWhileAStepIsOpen(t *testing.T) {
	ft := &fakeThread{}
	w := streamWriter(t, ft, "D1")

	w.queueAnswer(strings.Repeat("a", slackMarkdownBlockMax-1500) + " ")
	require.NoError(t, w.flush(t.Context()))
	w.queueStep(taskUpdate{id: "step-1", title: "Kubernetes get", status: stepInProgress, details: "x_kubernetes_get {}"})
	require.NoError(t, w.flush(t.Context()))
	w.queueNarration(strings.Repeat("n", 1500) + passageBreak)
	require.NoError(t, w.flush(t.Context()))
	w.queueStep(taskUpdate{id: "step-1", title: "Kubernetes get", status: stepComplete, output: "done"})
	require.NoError(t, w.flush(t.Context()))

	tss := ft.streamTSs()
	require.Len(t, tss, 1, "no roll-over while step-1 runs")
	require.Equal(t, stepComplete, ft.cardsOf(tss[0])["step-1"].status)
}

// A stream adopted after a restart held content even when its delivery record
// counts none, so Slack refusing its first append as too long moves the rest
// of the reply on to a new message instead of retrying it until the turn ends.
func TestStream_AdoptedStreamOverflowsOntoANewMessage(t *testing.T) {
	ft := &fakeThread{sizeLimit: slackMeasuredLimit}
	ft.streaming = map[string]bool{"adopted-1": true}
	ft.messages = map[string]capturedMessage{"adopted-1": {""}}
	ft.sizes = map[string]*fakeMessageSize{"adopted-1": {text: slackMeasuredLimit - 10}}
	w := streamWriter(t, ft, "D1")
	w.continueFrom(store.Delivered{StreamTS: "adopted-1"})

	w.queueAnswer("the rest of the answer, too long for the message it started on ")
	require.NoError(t, w.flush(t.Context()))

	require.Equal(t, 1, ft.refusedTooLong())
	require.Len(t, ft.streamTSs(), 1, "the rest opened a message of its own")
	require.Contains(t, ft.streamedText(), "the rest of the answer")
}

// A Stop press that Slack reports on the stop closing a full message ends the
// reply: the rest of it is dropped, not moved on to a new message.
func TestStream_OverflowAfterStopOpensNoNewMessage(t *testing.T) {
	ft := &fakeThread{sizeLimit: 1000, failStop: errCodeStoppedByUser}
	w := streamWriter(t, ft, "D1")

	w.queueAnswer(strings.Repeat("a", 900) + " ")
	require.NoError(t, w.flush(t.Context()))
	w.queueAnswer(strings.Repeat("b", 200) + " ")
	require.NoError(t, w.flush(t.Context()))

	require.Equal(t, 1, ft.refusedTooLong())
	require.Len(t, ft.streamTSs(), 1, "no message is opened after the Stop press")
	require.True(t, w.streamStopped)
}
