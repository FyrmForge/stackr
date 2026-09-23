package v1

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/config/settings"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// knob pulls one row out of a settings response.
func knob(t *testing.T, out settingsOut, key string) settingKnob {
	t.Helper()
	for _, v := range out.Values {
		if v.Key == key {
			return v
		}
	}
	t.Fatalf("no %s in %v", key, out.Values)
	return settingKnob{}
}

func TestSettingsPasswordNotInherited(t *testing.T) {
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	ctx := t.Context()
	on, user, pass := true, "admin", "hunter2"
	seed.Org.Settings = settings.Settings{Protect: &on, ProtectUser: &user, ProtectPassword: &pass}.JSON()
	require.NoError(t, store.UpdateOrg(ctx, seed.Org))

	a := &API{store: store, settings: service.NewSettingsService(store, nil), access: service.NewAccessService(store), nodeSvc: service.NewNodeService(store, nil, nil, "")}
	e := echo.New()
	c := e.NewContext(httptest.NewRequest("GET", "/", nil), httptest.NewRecorder())
	out, err := a.toSettingsOut(c, &settingsTarget{kind: "stack", org: seed.Org, stack: seed.Stack})
	require.NoError(t, err)

	assert.Equal(t, "1", knob(t, out, "protect").Value)
	assert.Equal(t, "admin", knob(t, out, "protect_user").Value, "who to log in as is not a secret")

	pw := knob(t, out, "protect_password")
	assert.Empty(t, pw.Value, "a stack reader must not be handed the org's password")
	assert.Equal(t, "org", pw.Source, "but they can see where it comes from")
	assert.Nil(t, pw.Own, "the stack sets none of its own")

	// Nor its own: reading settings takes only a read scope.
	out, err = a.toSettingsOut(c, &settingsTarget{kind: "org", org: seed.Org})
	require.NoError(t, err)
	pw = knob(t, out, "protect_password")
	assert.Empty(t, pw.Value)
	require.NotNil(t, pw.Own)
	assert.Equal(t, "set", *pw.Own, "a client can still tell set from inherited")
	assert.NotContains(t, *pw.Own, "hunter2")
}

// TestPatchSettingsRefusesHalfAPair: a user with no password resolves the
// password to empty and locks every URL below that level behind bytes nobody
// has. The save is where the operator can still fix it.
func TestPatchSettingsRefusesHalfAPair(t *testing.T) {
	store := testdb.New(t)
	testdb.SeedStack(t, store, false)
	a := &API{store: store, settings: service.NewSettingsService(store, nil), access: service.NewAccessService(store), nodeSvc: service.NewNodeService(store, nil, nil, "")}

	patch := func(body string) (int, string) {
		e := echo.New()
		req := httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(body))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		c := e.NewContext(req, httptest.NewRecorder())
		c.Set(ctxUser, &repo.User{ID: "u1", Role: "admin"})
		err := a.patchSettingsFor("server")(c)
		if he, ok := err.(*echo.HTTPError); ok {
			msg, _ := he.Message.(string)
			return he.Code, msg
		}
		require.NoError(t, err)
		return http.StatusOK, ""
	}

	code, msg := patch(`{"protect":"1","protect_user":"admin"}`)
	assert.Equal(t, http.StatusBadRequest, code)
	assert.Contains(t, msg, "password")

	sv, err := store.GetServer(t.Context(), "local")
	require.NoError(t, err)
	assert.Nil(t, settings.Parse(sv.Settings).ProtectUser, "a refused save writes nothing")

	code, _ = patch(`{"protect":"1","protect_user":"admin","protect_password":"hunter2"}`)
	assert.Equal(t, http.StatusOK, code)
}
