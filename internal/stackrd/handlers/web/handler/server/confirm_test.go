package server

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"
)

func postForm(body url.Values) echo.Context {
	r := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body.Encode()))
	r.Header.Set(echo.HeaderContentType, echo.MIMEApplicationForm)
	return echo.New().NewContext(r, httptest.NewRecorder())
}

// The Drain and Remove modals each rendered their guard as a
// `disabled` attribute on the submit button, and the handlers trusted it. A
// disabled attribute is a hint to a browser: curl, a replayed form or a page
// rendered before the state changed all sail past it. Draining a node a volume
// tile is pinned to took that tile down with nowhere to reschedule.
//
// The confirm boxes also had no name attribute, so nothing was ever submitted
// for a handler to check even if it had wanted to.
func TestConfirmedChecksWhatWasActuallyTyped(t *testing.T) {
	require.True(t, confirmed(postForm(url.Values{"confirm": {"worker-2"}}), "worker-2"))
	require.True(t, confirmed(postForm(url.Values{"confirm": {"  worker-2  "}}), "worker-2"),
		"a pasted name with whitespace is still the name")

	require.False(t, confirmed(postForm(url.Values{}), "worker-2"),
		"a form with no confirm field at all, which is what curl sends")
	require.False(t, confirmed(postForm(url.Values{"confirm": {""}}), "worker-2"))
	require.False(t, confirmed(postForm(url.Values{"confirm": {"skrt"}}), "worker-2"))
	require.False(t, confirmed(postForm(url.Values{"confirm": {"SKRT2"}}), "worker-2"),
		"the node's name, not a near miss")
	require.False(t, confirmed(postForm(url.Values{"confirm": {"worker-2"}}), ""),
		"an empty expectation must not be satisfiable")
	require.False(t, confirmed(postForm(url.Values{"confirm": {""}}), ""),
		"nor by sending nothing: a server row with no name is not a free pass")
}
