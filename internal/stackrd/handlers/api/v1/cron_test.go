package v1

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// createApp accepted cron tiles the scheduler could never run: no schedule
// validation, no image requirement. Both make a job that looks configured in
// the UI and silently never fires.
func TestCreateAppCronValidation(t *testing.T) {
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	a := apiFor(store)

	post := func(body string) (int, string) {
		e := echo.New()
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		rec := httptest.NewRecorder()
		c := e.NewContext(req, rec)
		c.SetParamNames("id")
		c.SetParamValues(seed.Stack.ID)
		c.Set(ctxUser, &repo.User{ID: "u1", Role: "admin"})
		err := a.createApp(c)
		if he, ok := err.(*echo.HTTPError); ok {
			msg, _ := he.Message.(string)
			return he.Code, msg
		}
		require.NoError(t, err, "unexpected error")
		return rec.Code, rec.Body.String()
	}

	code, msg := post(`{"name":"bad","kind":"cron","image":"alpine:3","schedule":"not a cron"}`)
	assert.Equal(t, http.StatusBadRequest, code, "invalid schedule: want 400, got %d %s", code, msg)
	code, msg = post(`{"name":"nosched","kind":"cron","image":"alpine:3"}`)
	assert.Equal(t, http.StatusBadRequest, code, "missing schedule: want 400, got %d %s", code, msg)
	// Crons build from git like services now, only a sourceless cron is refused.
	code, msg = post(`{"name":"gitcron","kind":"cron","schedule":"0 3 * * *","git_url":"https://github.com/x/y"}`)
	assert.Equal(t, http.StatusCreated, code, "git-source cron: want 201, got %d %s", code, msg)
	code, msg = post(`{"name":"nosource","kind":"cron","schedule":"0 3 * * *"}`)
	assert.Equal(t, http.StatusBadRequest, code, "cron without any source: want 400, got %d %s", code, msg)

	code, _ = post(`{"name":"good","kind":"cron","image":"alpine:3","schedule":"0 3 * * *"}`)
	require.Equal(t, http.StatusCreated, code, "valid cron: want 201, got %d", code)
	tile, err := store.GetTileBySlug(t.Context(), seed.Env.ID, "good")
	require.NoError(t, err, "created cron not found")
	require.NotNil(t, tile, "created cron not found")
	assert.Equal(t, 30, tile.TimeoutMinutes, "timeout should default to 30 like web/config")
}
