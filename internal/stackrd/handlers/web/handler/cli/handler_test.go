package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// fakeStore records the key minted at exchange; unimplemented methods panic
// (the embedded nil interface) but Exchange only touches CreateAPIKey.
type fakeStore struct {
	repo.Store
	created *repo.APIKey
}

func (f *fakeStore) CreateAPIKey(_ context.Context, k *repo.APIKey) error {
	f.created = k
	return nil
}

func TestValidPort(t *testing.T) {
	ok := []string{"1", "8080", "65535"}
	bad := []string{"", "0", "-1", "65536", "abc", "80x"}
	for _, s := range ok {
		assert.True(t, validPort(s), "validPort(%q) = false, want true", s)
	}
	for _, s := range bad {
		assert.False(t, validPort(s), "validPort(%q) = true, want false", s)
	}
}

func TestHostname(t *testing.T) {
	assert.Equal(t, "unknown host", hostname(""), "empty hostname")
	assert.Len(t, hostname(strings.Repeat("x", 100)), 64, "long hostname not truncated")
	assert.Equal(t, "deathstar", hostname("deathstar"), "hostname passthrough")
}

// exchangeStatus drives the Exchange handler with a JSON code body.
func exchangeStatus(t *testing.T, h *handler, code string) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/cli/exchange", strings.NewReader(`{"code":"`+code+`"}`))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	if err := h.Exchange(c); err != nil {
		he, ok := err.(*echo.HTTPError)
		require.True(t, ok, "Exchange: %v", err)
		return he.Code
	}
	return rec.Code
}

func TestExchangeMintsOnceThenBurns(t *testing.T) {
	store := &fakeStore{}
	h := NewHandler(store)
	h.codes["good"] = pending{userID: "u1", scopes: `["apps:read"]`, hostname: "host", expiry: time.Now().Add(time.Minute)}

	require.Equal(t, http.StatusOK, exchangeStatus(t, h, "good"), "first exchange")
	// Key is minted only now, at exchange, with the grant's user and scopes.
	if assert.NotNil(t, store.created, "minted key = nil, want user u1 / apps:read") {
		assert.Equal(t, "u1", store.created.UserID, "minted key = %+v, want user u1 / apps:read", store.created)
		assert.Equal(t, `["apps:read"]`, store.created.Scopes, "minted key = %+v, want user u1 / apps:read", store.created)
	}
	assert.Equal(t, http.StatusNotFound, exchangeStatus(t, h, "good"), "second exchange (code burned)")
}

func TestExchangeExpired(t *testing.T) {
	store := &fakeStore{}
	h := NewHandler(store)
	h.codes["stale"] = pending{userID: "u1", expiry: time.Now().Add(-time.Second)}
	assert.Equal(t, http.StatusNotFound, exchangeStatus(t, h, "stale"), "expired exchange")
	assert.Nil(t, store.created, "expired code must not mint a key")
}

func TestExchangeUnknown(t *testing.T) {
	h := NewHandler(nil)
	assert.Equal(t, http.StatusNotFound, exchangeStatus(t, h, "nope"), "unknown code")
}
