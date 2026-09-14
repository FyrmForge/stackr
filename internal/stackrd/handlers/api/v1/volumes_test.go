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

// volume_name goes into a bind string verbatim, so an unvalidated one is a
// host mount: "/" gives the container the node's root filesystem.
func TestCreateVolumeRefusesABindSource(t *testing.T) {
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	a := apiFor(store)
	app := seedService(t, store, seed)

	post := func(body string) (int, string) {
		e := echo.New()
		req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
		req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
		c := e.NewContext(req, httptest.NewRecorder())
		c.SetParamNames("id")
		c.SetParamValues(app.ID)
		c.Set(ctxUser, &repo.User{ID: "u1", Role: "admin"})
		err := a.createVolume(c)
		if he, ok := err.(*echo.HTTPError); ok {
			msg, _ := he.Message.(string)
			return he.Code, msg
		}
		require.NoError(t, err, "createVolume")
		return http.StatusCreated, ""
	}

	for _, name := range []string{"/", "/etc", "../../etc", "./x", "-x"} {
		code, msg := post(`{"name":"data","mount_path":"/data","volume_name":"` + name + `"}`)
		assert.Equal(t, http.StatusBadRequest, code, "volume_name %q was accepted: %s", name, msg)
	}

	// Empty is the normal case and means the id-derived stackr-vol-<id8>.
	assert.True(t, repo.ValidVolumeName("shared-data"))
	assert.False(t, repo.ValidVolumeName("/"))
}

// orgImage owns the status code: a traversal is the caller's bad path, so it
// is a 404, not the registry client's 502 repeating the namespaced name back.
func TestOrgImageRefusesTraversalAsNotFound(t *testing.T) {
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	a := apiFor(store)

	resolve := func(raw string) (string, int) {
		e := echo.New()
		c := e.NewContext(httptest.NewRequest(http.MethodGet, "/", nil), httptest.NewRecorder())
		c.SetParamNames("name")
		c.SetParamValues(raw)
		name, err := a.orgImage(c, seed.Org)
		if he, ok := err.(*echo.HTTPError); ok {
			return "", he.Code
		}
		require.NoError(t, err)
		return name, http.StatusOK
	}

	// echo 4 routes on the raw path and does not unescape Param, so the
	// decode is orgImage's job: without it these arrive as a literal "..".
	for _, raw := range []string{"%2e%2e%2fother_image", "..%2fother_image",
		"org_a%2f..%2f..%2fother_image", "org_%2e%2e"} {
		_, code := resolve(raw)
		assert.Equal(t, http.StatusNotFound, code, "accepted %q", raw)
	}

	// The friendly short form and the full one both resolve.
	full, code := resolve("org_shop_api")
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, "org_shop_api", full)
	short, code := resolve("shop_api")
	assert.Equal(t, http.StatusOK, code)
	assert.Equal(t, "org_shop_api", short, "the short form must resolve inside the namespace")
}
