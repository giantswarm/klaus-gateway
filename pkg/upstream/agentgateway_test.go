package upstream_test

import (
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/pkg/lifecycle"
	"github.com/giantswarm/klaus-gateway/pkg/upstream"
)

func TestAgentgateway_Apply(t *testing.T) {
	ag, err := upstream.Parse("http://agentgateway:8080/prefix")
	require.NoError(t, err)
	require.NotNil(t, ag)

	req := httptest.NewRequest("POST", "http://upstream.invalid/v1/chat/completions", nil)
	ag.Apply(req, lifecycle.InstanceRef{Name: "i1"})

	require.Equal(t, "agentgateway:8080", req.URL.Host)
	require.Equal(t, "http", req.URL.Scheme)
	require.Equal(t, "/prefix/v1/chat/completions", req.URL.Path)
	require.Equal(t, "i1", req.Header.Get(upstream.InstanceHeader))
}

func TestAgentgateway_EmptyIsDirect(t *testing.T) {
	ag, err := upstream.Parse("")
	require.NoError(t, err)
	require.Nil(t, ag)
}
