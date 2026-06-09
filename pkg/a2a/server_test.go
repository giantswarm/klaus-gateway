package a2a

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAgentRefMiddleware_Resolution(t *testing.T) {
	const defaultAgent = "default-worker"

	cases := []struct {
		name    string
		path    string
		header  string
		wantRef string
	}{
		{
			name:    "X-Agent-Ref header takes precedence over path",
			path:    "/a2a/worker-a",
			header:  "worker-b",
			wantRef: "worker-b",
		},
		{
			name:    "path segment used when no header",
			path:    "/a2a/worker-a",
			header:  "",
			wantRef: "worker-a",
		},
		{
			name:    "nested path uses first segment only",
			path:    "/a2a/worker-a/tasks/send",
			header:  "",
			wantRef: "worker-a",
		},
		{
			name:    "POST / falls back to defaultAgent",
			path:    "/",
			header:  "",
			wantRef: defaultAgent,
		},
		{
			name:    "/a2a with no trailing segment falls back to defaultAgent",
			path:    "/a2a",
			header:  "",
			wantRef: defaultAgent,
		},
		{
			name:    "/a2a/ trailing slash falls back to defaultAgent",
			path:    "/a2a/",
			header:  "",
			wantRef: defaultAgent,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var captured string
			inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				captured = AgentRefFromContext(r.Context())
				w.WriteHeader(http.StatusOK)
			})
			handler := agentRefMiddleware(inner, defaultAgent)

			req := httptest.NewRequest(http.MethodPost, tc.path, nil)
			if tc.header != "" {
				req.Header.Set("X-Agent-Ref", tc.header)
			}
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			require.Equal(t, http.StatusOK, rr.Code)
			require.Equal(t, tc.wantRef, captured)
		})
	}
}
