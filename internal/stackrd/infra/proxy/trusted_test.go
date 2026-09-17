package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestTrustedIPs(t *testing.T) {
	got := trustedIPs("10.0.0.0/8\n bogus \n173.245.48.0/20\n", "173.245.48.0/20\n2400:cb00::/32")
	assert.Equal(t, []string{"10.0.0.0/8", "173.245.48.0/20", "2400:cb00::/32"}, got)
	assert.Empty(t, trustedIPs("", ""))
}

func fakeCloudflare(t *testing.T, v4 string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ips-v4":
			_, _ = w.Write([]byte(v4))
		case "/ips-v6":
			_, _ = w.Write([]byte("2400:cb00::/32\n"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	old := cloudflareBase
	cloudflareBase = srv.URL
	t.Cleanup(func() { cloudflareBase = old })
}

func TestFetchCloudflare(t *testing.T) {
	fakeCloudflare(t, "173.245.48.0/20\n103.21.244.0/22\n")
	got, err := fetchCloudflare(context.Background())
	require.NoError(t, err)
	assert.Equal(t, []string{"173.245.48.0/20", "103.21.244.0/22", "2400:cb00::/32"}, got)
}

func TestFetchCloudflareRejectsGarbage(t *testing.T) {
	fakeCloudflare(t, "<html>maintenance</html>")
	_, err := fetchCloudflare(context.Background())
	assert.Error(t, err)
}

func TestFetchCloudflareUnreachable(t *testing.T) {
	old := cloudflareBase
	cloudflareBase = "http://127.0.0.1:1"
	t.Cleanup(func() { cloudflareBase = old })
	_, err := fetchCloudflare(context.Background())
	assert.Error(t, err)
}
