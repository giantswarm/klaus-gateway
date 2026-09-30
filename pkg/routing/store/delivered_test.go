package store_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/routing/store"
)

// A row an older image wrote still carries the step fields its delivery record
// had then. The bolt and valkey stores decode a row with encoding/json, so the
// row loads and keeps what a continuation needs: the text length and the open
// stream.
func TestEntry_DecodesARowWithTheRemovedStepFields(t *testing.T) {
	raw := `{"agent_ref":"kagent/sre-agent","task_id":"task-7","delivered":{"text_len":5,"stream_ts":"1.2","stream_len":5,"tool_steps":3,"open_step_id":"step-3","open_step_title":"Kubernetes list"}}`

	var e store.Entry
	require.NoError(t, json.Unmarshal([]byte(raw), &e))
	require.Equal(t, "task-7", e.TaskID)
	require.Equal(t, store.Delivered{TextLen: 5, StreamTS: "1.2", StreamLen: 5}, e.Delivered)
}
