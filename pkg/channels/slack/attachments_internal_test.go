package slack

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/channels"
)

func TestToInboundMessage_ImageWithoutCaption(t *testing.T) {
	event := slackInnerEvent{
		Type:    evtAppMention,
		User:    "U1",
		Channel: "C1",
		TS:      "1.2",
		Files: []slackFile{{
			Name:       "diagram.png",
			Mimetype:   "image/png",
			URLPrivate: "https://files.slack.com/diagram.png",
			Size:       4096,
		}},
	}
	msg, ok := event.toInboundMessage(false)
	require.True(t, ok, "an image with no caption must still become a turn")
	require.Empty(t, msg.Text)
	require.Len(t, msg.Attachments, 1)
	require.Equal(t, "diagram.png", msg.Attachments[0].Filename)
	require.Equal(t, "image/png", msg.Attachments[0].ContentType)
	require.Equal(t, "https://files.slack.com/diagram.png", msg.Attachments[0].SourceURL)
	require.Equal(t, 4096, msg.Attachments[0].Size)
}

func TestToInboundMessage_PrefersDownloadURL(t *testing.T) {
	event := slackInnerEvent{
		Type:    evtAppMention,
		User:    "U1",
		Channel: "C1",
		TS:      "1.2",
		Files: []slackFile{{
			Name:               "a.png",
			Mimetype:           "image/png",
			URLPrivate:         "https://files.slack.com/view",
			URLPrivateDownload: "https://files.slack.com/download",
		}},
	}
	msg, ok := event.toInboundMessage(false)
	require.True(t, ok)
	require.Equal(t, "https://files.slack.com/download", msg.Attachments[0].SourceURL)
}

func TestToInboundMessage_EmptyAndNoFilesDropped(t *testing.T) {
	event := slackInnerEvent{Type: evtAppMention, User: "U1", Channel: "C1", TS: "1.2"}
	_, ok := event.toInboundMessage(false)
	require.False(t, ok, "an empty message with no files is not a turn")
}

func TestSlackFileAttachments_SkipsFileWithoutURL(t *testing.T) {
	event := slackInnerEvent{
		Files: []slackFile{
			{Name: "no-url.png", Mimetype: "image/png"},
			{Name: "ok.png", Mimetype: "image/png", URLPrivate: "https://files.slack.com/ok.png"},
		},
	}
	atts := event.attachments()
	require.Len(t, atts, 1)
	require.Equal(t, "ok.png", atts[0].Filename)
}

func TestDownloadFile_SendsBearerAndReturnsBytes(t *testing.T) {
	payload := []byte("PNGDATA")
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	client := &slackAPIClient{botToken: "secret"}
	got, err := client.downloadFile(t.Context(), srv.URL, len(payload))
	require.NoError(t, err)
	require.Equal(t, "Bearer secret", gotAuth)
	require.Equal(t, payload, got)
}

func TestDownloadFile_RejectsBodyBeyondLimit(t *testing.T) {
	// Server returns far more than the declared size + margin.
	big := strings.Repeat("x", (2<<20)+downloadSizeMargin)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(big))
	}))
	defer srv.Close()

	client := &slackAPIClient{botToken: "t"}
	_, err := client.downloadFile(t.Context(), srv.URL, 1024) // declared 1KB, body is huge
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds")
}

func TestDownloadFile_ErrorsOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	client := &slackAPIClient{botToken: "t"}
	_, err := client.downloadFile(t.Context(), srv.URL, 10)
	require.Error(t, err)
	require.Contains(t, err.Error(), "403")
}

func TestAttachmentsTooLargeNote_QuotesTotalSize(t *testing.T) {
	note := attachmentsTooLargeNote([]channels.Attachment{
		{Bytes: make([]byte, 6<<20)},
		{Bytes: make([]byte, 6<<20)},
	})
	require.Contains(t, note, "12.0 MB")
	require.Contains(t, note, "too large")
	require.NotEqual(t, failedNote, note)
}
