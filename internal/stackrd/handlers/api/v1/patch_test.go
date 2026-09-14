package v1

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// patchTile PATCHes a tile with a raw JSON body and returns the reloaded row.
func patchTile(t *testing.T, a *API, store repo.Store, id, body string) (*repo.Tile, int, string) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(http.MethodPatch, "/", strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues(id)
	c.Set(ctxUser, &repo.User{ID: "u1", Role: "admin"})
	err := a.patchApp(c)
	if he, ok := err.(*echo.HTTPError); ok {
		msg, _ := he.Message.(string)
		return nil, he.Code, msg
	}
	require.NoError(t, err, "patchApp")
	got, gerr := store.GetTile(t.Context(), id)
	require.NoError(t, gerr, "reload")
	require.NotNil(t, got, "reload")
	return got, rec.Code, rec.Body.String()
}

func seedService(t *testing.T, store repo.Store, seed testdb.Seed) *repo.Tile {
	t.Helper()
	now := time.Now().UTC()
	tile := &repo.Tile{
		ID: "tile-patch", StackID: seed.Stack.ID, EnvironmentID: seed.Env.ID,
		Name: "api", Slug: "api", Kind: "service", SourceType: "image", ImageRef: "nginx:1",
		ContainerPort: 8080, CPULimit: 1.5, MemLimitMB: 512, BuildArgs: "A=1",
		WatchPaths: "^src/", Status: "idle", CreatedAt: now, UpdatedAt: now,
	}
	require.NoError(t, store.CreateTile(t.Context(), tile), "seed tile")
	return tile
}

// The reason patchApp reads the body twice. TileConf is value-typed, so a
// one-field patch decodes every other field as its zero value, bind it
// straight and `{"image":"x"}` silently blanks the port, the limits and the
// build args. Only keys the caller actually sent may be written.
func TestPatchAppLeavesUnmentionedFieldsAlone(t *testing.T) {
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	a := apiFor(store)
	tile := seedService(t, store, seed)

	got, code, msg := patchTile(t, a, store, tile.ID, `{"image":"nginx:2"}`)
	require.Equal(t, http.StatusOK, code, "patch: %d %s", code, msg)
	assert.Equal(t, "nginx:2", got.ImageRef, "image not written")
	assert.True(t, got.ContainerPort == 8080 && got.CPULimit == 1.5 && got.MemLimitMB == 512,
		"a one-field patch zeroed other fields: port=%d cpu=%g mem=%d",
		got.ContainerPort, got.CPULimit, got.MemLimitMB)
	assert.True(t, got.BuildArgs == "A=1" && got.WatchPaths == "^src/",
		"a one-field patch cleared strings: build_args=%q watch_paths=%q",
		got.BuildArgs, got.WatchPaths)
}

// The other half of the same rule: a key that IS present writes, including when
// its value is the zero one. Without this "clear the cpu limit" is unsayable.
func TestPatchAppWritesExplicitZeroes(t *testing.T) {
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	a := apiFor(store)
	tile := seedService(t, store, seed)

	got, code, msg := patchTile(t, a, store, tile.ID,
		`{"limits":{"cpu":0,"memory_mb":0},"build_args":"","port":0}`)
	require.Equal(t, http.StatusOK, code, "patch: %d %s", code, msg)
	assert.True(t, got.CPULimit == 0 && got.MemLimitMB == 0,
		"limits not cleared: cpu=%g mem=%d", got.CPULimit, got.MemLimitMB)
	assert.Equal(t, "", got.BuildArgs, "build_args not cleared")
	assert.Equal(t, 0, got.ContainerPort, "port not cleared")
}

// The widened body reaches the settings fields the old four-field patch could
// not, and `git_branch` still works alongside TileConf's `branch`.
func TestPatchAppWidenedFields(t *testing.T) {
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	a := apiFor(store)
	tile := seedService(t, store, seed)

	got, code, msg := patchTile(t, a, store, tile.ID, `{
		"healthcheck": "curl -f localhost:8080/healthz",
		"published_ports": "9000:9000",
		"traefik_override": "http: {}",
		"security_headers": true,
		"watch_paths": ["^cmd/", "!^docs/"],
		"build": {"dockerfile": "cmd/api/Dockerfile", "context": "."},
		"git_branch": "release"
	}`)
	require.Equal(t, http.StatusOK, code, "patch: %d %s", code, msg)
	assert.True(t, got.HealthcheckCmd != "" && got.PublishedPorts == "9000:9000" && got.TraefikOverride == "http: {}",
		"routing fields not written: %+v", got)
	assert.True(t, got.SecHeaders, "bool fields not written: sec=%v", got.SecHeaders)
	assert.Equal(t, "^cmd/\n!^docs/", got.WatchPaths, "watch_paths, want the lines joined")
	assert.True(t, got.DockerfilePath == "cmd/api/Dockerfile" && got.BuildContext == ".",
		"build block not written: %q %q", got.DockerfilePath, got.BuildContext)
	assert.Equal(t, "release", got.GitBranch, "the legacy git_branch key stopped working")
}

// The runtime fields land on service tiles, are validated, and stay off crons.
func TestPatchAppRuntimeFields(t *testing.T) {
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	a := apiFor(store)
	tile := seedService(t, store, seed)

	got, code, msg := patchTile(t, a, store, tile.ID, `{
		"command": "-config.file=/etc/loki/config.yml",
		"user": "1000:1000",
		"shm_size_mb": 128,
		"privileged": true,
		"devices": ["/dev/kmsg"],
		"restart": "always"
	}`)
	require.Equal(t, http.StatusOK, code, "patch: %d %s", code, msg)
	assert.Equal(t, "-config.file=/etc/loki/config.yml", got.Command, "command not written")
	assert.Equal(t, "1000:1000", got.User, "user not written")
	assert.Equal(t, 128, got.ShmSizeMB, "shm_size_mb not written")
	assert.True(t, got.Privileged, "privileged not written")
	assert.Equal(t, "/dev/kmsg", got.Devices, "devices not written")
	assert.Equal(t, "", got.RestartPolicy, "restart=always is the default and stores as empty")

	got, code, _ = patchTile(t, a, store, tile.ID, `{"restart":"no"}`)
	assert.Equal(t, http.StatusOK, code, "restart=no: want 200, got %d", code)
	assert.Equal(t, "no", got.RestartPolicy, "restart=no not written")
	_, code, _ = patchTile(t, a, store, tile.ID, `{"restart":"sometimes"}`)
	assert.Equal(t, http.StatusBadRequest, code, "restart=sometimes: want 400, got %d", code)
	_, code, _ = patchTile(t, a, store, tile.ID, `{"devices":["kmsg"]}`)
	assert.Equal(t, http.StatusBadRequest, code, "relative device path: want 400, got %d", code)
	_, code, _ = patchTile(t, a, store, tile.ID, `{"shm_size_mb":-1}`)
	assert.Equal(t, http.StatusBadRequest, code, "negative shm: want 400, got %d", code)

	now := time.Now().UTC()
	cron := &repo.Tile{ID: "tile-cron-rt", StackID: seed.Stack.ID, EnvironmentID: seed.Env.ID,
		Name: "nightly2", Slug: "nightly2", Kind: "cron", SourceType: "image", ImageRef: "alpine:3",
		Cron: "0 3 * * *", TimeoutMinutes: 30, Status: "idle", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, store.CreateTile(t.Context(), cron), "seed cron")
	_, code, _ = patchTile(t, a, store, cron.ID, `{"privileged":true}`)
	assert.Equal(t, http.StatusBadRequest, code, "runtime fields on a cron: want 400, got %d", code)
	// command is shared now: a cron keeps taking it.
	gotCron, code, msg := patchTile(t, a, store, cron.ID, `{"command":"sh /x.sh"}`)
	require.Equal(t, http.StatusOK, code, "cron command: %d %s", code, msg)
	assert.Equal(t, "sh /x.sh", gotCron.Command, "cron command not written")
}

// A schedule that does not parse is skipped by LoadSchedules, so the job looks
// configured and never fires, the same failure createApp already guards, and
// the one stackconf's updateTile once shipped.
func TestPatchAppCronGuards(t *testing.T) {
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	a := apiFor(store)
	tile := seedService(t, store, seed)

	_, code, _ := patchTile(t, a, store, tile.ID, `{"schedule":"0 3 * * *"}`)
	assert.Equal(t, http.StatusBadRequest, code, "cron fields on a service tile: want 400, got %d", code)

	// A fresh row rather than flipping Kind on the existing one: UpdateTile does
	// not write Kind, so the tile would still read back as a service.
	now := time.Now().UTC()
	cron := &repo.Tile{ID: "tile-cron", StackID: seed.Stack.ID, EnvironmentID: seed.Env.ID,
		Name: "nightly", Slug: "nightly", Kind: "cron", SourceType: "image", ImageRef: "alpine:3",
		Cron: "0 3 * * *", TimeoutMinutes: 30, Status: "idle", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, store.CreateTile(t.Context(), cron), "seed cron")
	tile = cron
	_, code, _ = patchTile(t, a, store, tile.ID, `{"schedule":"not a cron"}`)
	assert.Equal(t, http.StatusBadRequest, code, "invalid schedule: want 400, got %d", code)
	got, code, msg := patchTile(t, a, store, tile.ID, `{"schedule":"*/5 * * * *","allow_overlap":true}`)
	require.Equal(t, http.StatusOK, code, "valid schedule: %d %s", code, msg)
	assert.True(t, got.Cron == "*/5 * * * *" && got.AllowOverlap,
		"cron fields not written: %q %v", got.Cron, got.AllowOverlap)
}

// A connector carries git credentials, so one from another org must be refused
// rather than silently dropped, the caller asked for specific access.
func TestPatchAppRejectsForeignConnector(t *testing.T) {
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	a := apiFor(store)
	tile := seedService(t, store, seed)

	other := &repo.Org{ID: "org-other", Name: "Other", Slug: "other"}
	require.NoError(t, store.CreateOrg(t.Context(), other), "org")
	cn := &repo.Connector{ID: "conn-foreign", OrgID: other.ID, Provider: "github",
		Name: "theirs", CreatedAt: time.Now().UTC()}
	require.NoError(t, store.CreateConnector(t.Context(), cn), "connector")

	_, code, _ := patchTile(t, a, store, tile.ID, `{"connector":"conn-foreign"}`)
	assert.Equal(t, http.StatusBadRequest, code, "foreign connector: want 400, got %d", code)
	_, code, _ = patchTile(t, a, store, tile.ID, `{"connector":"nope"}`)
	assert.Equal(t, http.StatusBadRequest, code, "unknown connector: want 400, got %d", code)
}
