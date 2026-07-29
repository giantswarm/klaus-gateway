package slack

import (
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
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

// A message with an upload arrives with subtype file_share; it is a human
// message and must route, unlike every other subtype — even one carrying files.
func TestToInboundMessage_FileShareSubtypeRoutes(t *testing.T) {
	event := slackInnerEvent{
		Type:     evtMessage,
		SubType:  subtypeFileShare,
		User:     "U1",
		Channel:  "C1",
		TS:       "2.2",
		ThreadTS: "1.1",
		Files: []slackFile{{
			Name:       "diagram.png",
			Mimetype:   "image/png",
			URLPrivate: "https://files.slack.com/diagram.png",
			Size:       4096,
		}},
	}
	msg, ok := event.toInboundMessage(true)
	require.True(t, ok, "a file_share thread reply must become a turn")
	require.Len(t, msg.Attachments, 1)
	require.Equal(t, "diagram.png", msg.Attachments[0].Filename)

	event.SubType = "message_changed"
	_, ok = event.toInboundMessage(true)
	require.False(t, ok, "other subtypes stay rejected even when they carry files")
}

func TestToInboundMessage_EmptyAndNoFilesDropped(t *testing.T) {
	event := slackInnerEvent{Type: evtAppMention, User: "U1", Channel: "C1", TS: "1.2"}
	_, ok := event.toInboundMessage(false)
	require.False(t, ok, "an empty message with no files is not a turn")
}

func TestSlackFileAttachments_KeepsFileWithoutURLForNotice(t *testing.T) {
	event := slackInnerEvent{
		Files: []slackFile{
			{Name: "no-url.png", Mimetype: "image/png"},
			{Name: "ok.png", Mimetype: "image/png", URLPrivate: "https://files.slack.com/ok.png"},
		},
	}
	atts := event.attachments()
	require.Len(t, atts, 2, "a URL-less file stays listed so the dropped-attachments notice can name it")
	require.Equal(t, "no-url.png", atts[0].Filename)
	require.Empty(t, atts[0].SourceURL)
	require.Equal(t, "ok.png", atts[1].Filename)
	require.Equal(t, "https://files.slack.com/ok.png", atts[1].SourceURL)
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
	got, err := client.downloadFile(t.Context(), srv.URL, "image/png", len(payload))
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
	_, err := client.downloadFile(t.Context(), srv.URL, "image/png", 1024) // declared 1KB, body is huge
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds")
}

func TestDownloadFile_ErrorsOnNon2xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
	}))
	defer srv.Close()

	client := &slackAPIClient{botToken: "t"}
	_, err := client.downloadFile(t.Context(), srv.URL, "image/png", 10)
	require.Error(t, err)
	require.Contains(t, err.Error(), "403")
}

// TestDownloadFile_RejectsSignInPage covers Slack answering an unauthorized
// url_private with HTTP 200 and its web sign-in page. Detection is by body (the
// Content-Type varies), so the login page is never forwarded as file bytes.
func TestDownloadFile_RejectsSignInPage(t *testing.T) {
	// Served with a non-HTML Content-Type, as the download path does in practice,
	// to prove detection does not rely on the Content-Type header.
	for _, ct := range []string{"text/html; charset=utf-8", "application/force-download", "text/plain"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", ct)
			_, _ = w.Write([]byte(`<!DOCTYPE html><html lang="en-US" class="" data-primer data-cdn="https://a.slack-edge.com/"><head></head></html>`))
		}))
		client := &slackAPIClient{botToken: "t"}
		_, err := client.downloadFile(t.Context(), srv.URL, "image/png", 1024)
		srv.Close()
		require.Error(t, err, "content-type %q", ct)
		require.Contains(t, err.Error(), "sign-in page")
	}
}

// TestDownloadFile_AllowsGenuineHTMLFile ensures a real .html upload is still
// accepted: the sign-in guard keys on Slack-specific page markers, not on HTML
// alone and not on a generic sign-in link in the user's own page.
func TestDownloadFile_AllowsGenuineHTMLFile(t *testing.T) {
	payload := []byte(`<html><body>real upload, even with <a href="/signin">its own signin link</a></body></html>`)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	client := &slackAPIClient{botToken: "t"}
	got, err := client.downloadFile(t.Context(), srv.URL, "text/html", len(payload))
	require.NoError(t, err)
	require.Equal(t, payload, got)
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

func TestDownloadFile_UnknownSize_AllowsBeyondMargin(t *testing.T) {
	// A file whose declared size is unknown (0) is not capped at the margin
	// alone: a body larger than the margin but under the ceiling is accepted.
	payload := strings.Repeat("x", downloadSizeMargin+4096)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()

	client := &slackAPIClient{botToken: "t"}
	got, err := client.downloadFile(t.Context(), srv.URL, "image/png", 0)
	require.NoError(t, err)
	require.Len(t, got, len(payload))
}

func TestDownloadFile_UnknownSize_RejectsBeyondCeiling(t *testing.T) {
	big := strings.Repeat("x", unknownSizeDownloadLimit+downloadSizeMargin)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(big))
	}))
	defer srv.Close()

	client := &slackAPIClient{botToken: "t"}
	_, err := client.downloadFile(t.Context(), srv.URL, "image/png", 0)
	require.Error(t, err)
	require.Contains(t, err.Error(), "exceeds")
}

func TestIsSlackFileURL(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
		want bool
	}{
		{"files subdomain", "https://files.slack.com/x.png", true},
		{"apex host", "https://slack.com/x.png", true},
		{"http rejected", "http://files.slack.com/x.png", false},
		{"foreign host", "https://evil.example/x.png", false},
		{"suffix spoof", "https://files.slack.com.evil.example/x.png", false},
		{"empty", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, isSlackFileURL(tc.raw))
		})
	}
}

func TestSlackFileAttachments_ClearsNonSlackURL(t *testing.T) {
	event := slackInnerEvent{
		Files: []slackFile{
			{Name: "evil.png", Mimetype: "image/png", URLPrivate: "https://evil.example/steal.png"},
			{Name: "ok.png", Mimetype: "image/png", URLPrivate: "https://files.slack.com/ok.png"},
		},
	}
	atts := event.attachments()
	require.Len(t, atts, 2)
	require.Empty(t, atts[0].SourceURL, "a non-Slack URL must never reach the downloader, which sends the bot token")
	require.Equal(t, "https://files.slack.com/ok.png", atts[1].SourceURL)
}

func TestDroppedAttachmentsNote_NamesFiles(t *testing.T) {
	note := droppedAttachmentsNote([]string{"a.png", "b.pdf"})
	require.Contains(t, note, "a.png")
	require.Contains(t, note, "b.pdf")
}

func TestPayloadTooLargeNote_DistinctNotices(t *testing.T) {
	require.NotEqual(t, failedNote, payloadTooLargeNote)
	require.NotEqual(t, attachmentsUnavailableNote, payloadTooLargeNote)
}

func TestDownloadFile_RefusesDeclaredOversizeWithoutFetching(t *testing.T) {
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, _ *http.Request) {
		requests.Add(1)
	}))
	defer srv.Close()

	client := &slackAPIClient{botToken: "t"}
	_, err := client.downloadFile(t.Context(), srv.URL, "video/mp4", maxAttachmentDownload+1)
	require.Error(t, err)
	require.Contains(t, err.Error(), "ceiling")
	require.Zero(t, requests.Load(), "an oversize file is refused before any bytes are fetched")
}

// TestDownloadFile_RejectsSignInPageByTitle covers a sign-in page variant that
// carries none of the asset markers, only the page title.
func TestDownloadFile_RejectsSignInPageByTitle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte(`<html><head><title>Sign in to Slack</title></head><body></body></html>`))
	}))
	defer srv.Close()

	client := &slackAPIClient{botToken: "t"}
	_, err := client.downloadFile(t.Context(), srv.URL, "image/png", 1024)
	require.Error(t, err)
	require.Contains(t, err.Error(), "sign-in page")
}

// TestFetchAttachments_KeepsOrderAndDropsFailures pins the assembly semantics
// of the concurrent download path: attachment order is preserved, a failed or
// URL-less file is dropped by name, and the rest carry their bytes.
func TestFetchAttachments_KeepsOrderAndDropsFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/a":
			_, _ = w.Write([]byte("AAA"))
		case "/fail":
			w.WriteHeader(http.StatusForbidden)
		case "/c":
			_, _ = w.Write([]byte("CCC"))
		}
	}))
	defer srv.Close()

	a := &Adapter{Logger: slog.Default()}
	msg := channels.InboundMessage{Attachments: []channels.Attachment{
		{Filename: "a.txt", SourceURL: srv.URL + "/a"},
		{Filename: "external.png"}, // URL cleared at intake (non-Slack host)
		{Filename: "fail.png", SourceURL: srv.URL + "/fail"},
		{Filename: "c.txt", SourceURL: srv.URL + "/c"},
	}}
	dropped := a.fetchAttachments(t.Context(), &msg)
	require.Equal(t, []string{"external.png", "fail.png"}, dropped)
	require.Len(t, msg.Attachments, 2)
	require.Equal(t, "a.txt", msg.Attachments[0].Filename)
	require.Equal(t, []byte("AAA"), msg.Attachments[0].Bytes)
	require.Equal(t, "c.txt", msg.Attachments[1].Filename)
	require.Equal(t, []byte("CCC"), msg.Attachments[1].Bytes)
}
