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
// around a Kubernetes object.
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

// A tool-heavy turn — seventeen calls with object-sized results, the turn
// Slack refused as msg_too_long on gazelle (2026-09-28) while its calls rode
// the reply as steps — now puts only its prose on the reply, so it lands in
// one message with room to spare.
func TestStream_ToolCallsDoNotCountTowardTheMessageSize(t *testing.T) {
	ft := &fakeThread{sizeLimit: slackMeasuredLimit}
	_, _, err := runSurfaceWriter(t, ft, "C1", stuckDeletingTurn()...)
	require.NoError(t, err)

	require.Zero(t, ft.refusedTooLong())
	require.Len(t, ft.streamTSs(), 1, "the whole reply fits one message")
	require.Contains(t, ft.streamedText(), answerMarkdown(), "the whole answer is delivered")
	for _, typ := range ft.chunkOrder() {
		require.Equal(t, chunkTypeMarkdownText, typ, "only prose goes on the reply")
	}
}

// The size budget is an estimate of a limit Slack does not document. Should
// Slack still refuse an append as too long, the full message is closed and the
// rest of the reply continues on a new one.
func TestStream_MsgTooLongContinuesOnANewMessage(t *testing.T) {
	ft := &fakeThread{sizeLimit: 4000} // well under the writer's budget
	w := streamWriter(t, ft, "D1")

	w.queueAnswer(strings.Repeat("a", 3500) + " ")
	require.NoError(t, w.flush(t.Context()))
	w.queueAnswer(strings.Repeat("b", 1000) + " ")
	require.NoError(t, w.flush(t.Context()), "a refusal the reply recovers from is not a failure")
	w.queueAnswer(strings.Repeat("c", 1000) + " ")
	require.NoError(t, w.flush(t.Context()))

	require.Equal(t, 1, ft.refusedTooLong(), "the size the full message held bounds the messages after it")
	require.Len(t, ft.streamTSs(), 2, "the rest of the reply opened a message of its own")
	require.Equal(t, []string{methodChatStartStream, methodChatAppendStream, methodChatStopStream,
		methodChatStartStream, methodChatAppendStream}, ft.streamMethods())
	require.Equal(t, strings.Repeat("b", 1000)+" "+strings.Repeat("c", 1000)+" ", ft.finalMessages()[1][0])
}

// A stream adopted after a restart held content even when its delivery record
// counts none, so Slack refusing its first append as too long moves the rest
// of the reply on to a new message instead of retrying it until the turn ends.
func TestStream_AdoptedStreamOverflowsOntoANewMessage(t *testing.T) {
	ft := &fakeThread{sizeLimit: slackMeasuredLimit}
	ft.streaming = map[string]bool{"adopted-1": true}
	ft.messages = map[string]capturedMessage{"adopted-1": {""}}
	ft.sizes = map[string]int{"adopted-1": slackMeasuredLimit - 10}
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
