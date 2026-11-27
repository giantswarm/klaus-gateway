package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/require"

	"github.com/giantswarm/klaus-gateway/internal/config"
)

func TestPlatformVerifierKeepsProxyPrivateAddressAndCATrust(t *testing.T) {
	for name, test := range map[string]struct {
		target       string
		allowPrivate bool
		trustCA      bool
		noProxy      bool
		httpOnly     bool
		wantProxy    int32
		wantSuccess  bool
	}{
		"public target through HTTPS proxy":  {target: "203.0.113.1", trustCA: true, wantProxy: 1, wantSuccess: true},
		"private target through HTTPS proxy": {target: "10.0.0.1", allowPrivate: true, trustCA: true, wantProxy: 1, wantSuccess: true},
		"unlisted private target":            {target: "10.0.0.1", trustCA: true},
		"private CA is required":             {target: "10.0.0.1", allowPrivate: true, wantProxy: 1},
		"NO_PROXY bypasses the proxy":        {target: "10.0.0.1", allowPrivate: true, trustCA: true, noProxy: true},
		"HTTP_PROXY alone is not HTTPS":      {target: "10.0.0.1", allowPrivate: true, trustCA: true, httpOnly: true},
	} {
		t.Run(name, func(t *testing.T) {
			endpoint, caFile, signed, proxyURL, connects := proxyKeySet(t, test.target)
			for _, variable := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"} {
				t.Setenv(variable, "")
			}
			if test.httpOnly {
				t.Setenv("HTTP_PROXY", proxyURL)
			} else {
				t.Setenv("HTTPS_PROXY", proxyURL)
			}
			if test.noProxy {
				t.Setenv("NO_PROXY", test.target)
			}
			if !test.trustCA {
				caFile = ""
			}
			token := config.PlatformToken{Issuer: "https://dex.example.test", JWKSURL: endpoint, Audience: "klaus-gateway", AllowedClients: []string{"exchange-client"}}
			if test.allowPrivate {
				token.AllowPrivateIPJWKSHosts = []string{test.target}
			}
			verifier, err := platformVerifier(token, caFile)
			require.NoError(t, err)
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			defer cancel()
			_, err = verifier.Verify(ctx, signed)
			if test.wantSuccess {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				if !test.trustCA {
					var untrusted x509.UnknownAuthorityError
					require.ErrorAs(t, err, &untrusted)
				}
			}
			require.Equal(t, test.wantProxy, connects.Load())
		})
	}
}

func proxyKeySet(t *testing.T, target string) (endpoint, caFile, signed, proxyURL string, connects *atomic.Int32) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	certificate := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test identity provider"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour),
		IPAddresses: []net.IP{net.ParseIP(target)}, IsCA: true, BasicConstraintsValid: true,
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	require.NoError(t, err)
	caFile = filepath.Join(t.TempDir(), "ca.crt")
	require.NoError(t, os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	keys, err := json.Marshal(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{Key: &key.PublicKey, KeyID: "test", Algorithm: string(jose.RS256), Use: "sig"}}})
	require.NoError(t, err)
	backend := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/keys" {
			http.NotFound(w, request)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(keys)
	}))
	backend.TLS = &tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}}
	backend.StartTLS()
	t.Cleanup(backend.Close)
	connects = &atomic.Int32{}
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodConnect || request.Host != net.JoinHostPort(target, "443") {
			http.Error(w, "unexpected proxy target", http.StatusBadRequest)
			return
		}
		connects.Add(1)
		upstream, err := net.Dial("tcp", backend.Listener.Addr().String())
		if err != nil {
			http.Error(w, "backend unavailable", http.StatusBadGateway)
			return
		}
		defer func() { _ = upstream.Close() }()
		connection, buffered, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = connection.Close() }()
		_, _ = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		_ = buffered.Flush()
		go func() { _, _ = io.Copy(upstream, buffered) }()
		_, _ = io.Copy(connection, upstream)
	}))
	t.Cleanup(proxy.Close)
	signer, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, (&jose.SignerOptions{}).WithHeader(jose.HeaderKey("kid"), "test"))
	require.NoError(t, err)
	payload, err := json.Marshal(map[string]any{"iss": "https://dex.example.test", "sub": "test", "aud": "klaus-gateway", "azp": "exchange-client", "iat": time.Now().Unix(), "exp": time.Now().Add(time.Hour).Unix()})
	require.NoError(t, err)
	object, err := signer.Sign(payload)
	require.NoError(t, err)
	signed, err = object.CompactSerialize()
	require.NoError(t, err)
	return "https://" + target + "/keys", caFile, signed, proxy.URL, connects
}
