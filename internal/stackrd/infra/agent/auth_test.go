package agent

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

// Auth used to wrap each route, so the mux's method check answered
// first. A keyless GET of a POST route returned 405 and a keyless GET of a
// route that does not exist returned 404, which let anyone already on the
// overlay map the agent's whole surface without a key.
//
// RT stays nil on purpose: nothing unauthenticated may reach a handler, so a
// nil runtime would panic if one ever did.
func TestAgentRefusesBeforeRouting(t *testing.T) {
	h := (&Server{Key: "right", Version: "test"}).Handler()

	cases := []struct{ method, path string }{
		{http.MethodGet, PathInfo},          // a POST route, asked with GET
		{http.MethodPost, PathInfo},         // the real method
		{http.MethodGet, PathVolumeRead},    // a real GET route
		{http.MethodPost, PathExecShell},    // the sharpest one
		{http.MethodPost, PathVolumeRemove}, // and the most destructive
		{http.MethodGet, "/v1/does-not-exist"},
		{http.MethodPost, "/"},
	}
	for _, c := range cases {
		for _, key := range []string{"", "wrong"} {
			r := httptest.NewRequest(c.method, c.path, nil)
			if key != "" {
				r.Header.Set("Authorization", "Bearer "+key)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			require.Equal(t, http.StatusUnauthorized, w.Code,
				"%s %s with key %q must 401 before anything else", c.method, c.path, key)
		}
	}
}

// The liveness probe swarm itself calls stays outside auth, and "GET /healthz"
// must keep beating the catch-all "/" pattern.
func TestAgentHealthzStaysOpen(t *testing.T) {
	h := (&Server{Key: "right", Version: "test"}).Handler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	require.Equal(t, http.StatusOK, w.Code)
	require.Equal(t, "ok", w.Body.String())
}

// An empty key must not become a skeleton key for callers that send none.
func TestAgentWithNoKeyConfiguredRefusesEveryone(t *testing.T) {
	h := (&Server{Version: "test"}).Handler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, PathInfo, nil))
	require.Equal(t, http.StatusUnauthorized, w.Code)
}
