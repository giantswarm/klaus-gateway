package slack

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

// A Socket Mode events_api payload carries the same event_id as the Events API
// callback. Parsing it is what lets handleInbound dedup Socket Mode deliveries;
// before this, the id was dropped and only the Events API path deduped (#131).
func TestSocketModePayloadCarriesEventID(t *testing.T) {
	raw := []byte(`{
		"event_id":"Ev-socket",
		"event":{"type":"app_mention","user":"U1","text":"<@BOT> hi","channel":"C1","ts":"1.2"}
	}`)

	var payload smEventPayload
	require.NoError(t, json.Unmarshal(raw, &payload))
	require.Equal(t, "Ev-socket", payload.EventID)
	require.Equal(t, "app_mention", payload.Event.Type)
}
