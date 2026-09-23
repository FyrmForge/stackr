package v1

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo/sqlite"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// The removed backups feature resolved every route by backup id and never
// checked the tile's org: any authenticated user could run, delete or restore
// another org's backup. These tests are the standing proof that it cannot come
// back, plus the two holes a plain tile→org check does not close (a
// destination belonging to another org, and a run belonging to another
// backup).

// callAs is `call` for a non-admin user with an explicit set of org ids.
func callAs(t *testing.T, a *API, h echo.HandlerFunc, method, target, body, id string, orgIDs []string, scopes ...string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	e := echo.New()
	req := httptest.NewRequest(method, target, strings.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetParamNames("id")
	c.SetParamValues(id)
	scopeJSON, _ := json.Marshal(scopes)
	c.Set(ctxKey, &repo.APIKey{Scopes: string(scopeJSON)})
	c.Set(ctxUser, &repo.User{ID: "outsider", Role: "user"})
	ids := map[string]bool{}
	for _, o := range orgIDs {
		ids[o] = true
	}
	c.Set(ctxOrgIDs, ids)
	return rec, h(c)
}

// callGated is callAs through the route's gate, which is where authorization
// lives now. A handler called bare asserts nothing about who is asking — that
// is the point of point 18 — so a test that calls one bare proves nothing.
func callGated(t *testing.T, a *API, h echo.HandlerFunc, method, target, body, id string,
	orgIDs []string, v service.Verb, k service.Kind, scopes ...string) (*httptest.ResponseRecorder, error) {
	t.Helper()
	return callAs(t, a, a.gate(v, k, "id", h), method, target, body, id, orgIDs, scopes...)
}

type twoOrgs struct {
	seed     testdb.Seed // org1 / stack1 / tile1
	otherOrg *repo.Org
	otherDB  *repo.Tile
	destA    *repo.BackupDestination // org1's
	destB    *repo.BackupDestination // otherOrg's
	backupA  *repo.Backup            // on org1's tile
	runA     *repo.BackupRun
	backupB  *repo.Backup // on otherOrg's tile
}

func seedTwoOrgs(t *testing.T, s *sqlite.Store) twoOrgs {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	seed := testdb.SeedStack(t, s, false)

	org2 := &repo.Org{ID: "org2", Name: "Other", Slug: "other", CreatedAt: now, SetupDoneAt: &now}
	require.NoError(t, s.CreateOrg(ctx, org2), "create org2")
	st2 := &repo.Stack{ID: "stack2", OrgID: org2.ID, Name: "S2", Slug: "s2", CreatedAt: now}
	require.NoError(t, s.CreateStack(ctx, st2), "create stack2")
	env2 := &repo.Environment{ID: "env2", StackID: st2.ID, Name: "prod", Slug: "prod", Type: "static", CreatedAt: now}
	require.NoError(t, s.CreateEnvironment(ctx, env2), "create env2")
	db2 := &repo.Tile{ID: "tile2", StackID: st2.ID, EnvironmentID: env2.ID, Name: "pg", Slug: "pg",
		Kind: "service", Engine: "postgres", Status: "idle", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, s.CreateTile(ctx, db2), "create tile2")

	destA := &repo.BackupDestination{ID: "destA", OrgID: sql.NullString{String: seed.Org.ID, Valid: true},
		Name: "a", Endpoint: "https://s3", Bucket: "a", CreatedAt: now}
	destB := &repo.BackupDestination{ID: "destB", OrgID: sql.NullString{String: org2.ID, Valid: true},
		Name: "b", Endpoint: "https://s3", Bucket: "b", CreatedAt: now}
	for _, d := range []*repo.BackupDestination{destA, destB} {
		require.NoError(t, s.CreateBackupDestination(ctx, d), "create destination")
	}

	backupA := &repo.Backup{ID: "bkA", TileID: sql.NullString{String: seed.Tile.ID, Valid: true},
		DestinationID: destA.ID, Kind: repo.BackupVolume, ContainerMode: repo.ModePause,
		Cron: "0 3 * * *", KeepLatest: 3, Enabled: true, CreatedAt: now}
	backupB := &repo.Backup{ID: "bkB", TileID: sql.NullString{String: db2.ID, Valid: true},
		DestinationID: destB.ID, Kind: repo.BackupDump, Cron: "0 4 * * *", KeepLatest: 3, Enabled: true, CreatedAt: now}
	for _, b := range []*repo.Backup{backupA, backupB} {
		require.NoError(t, s.CreateBackup(ctx, b), "create backup")
	}
	runA := &repo.BackupRun{ID: "runA", BackupID: backupA.ID, Trigger: "manual", Status: "done",
		ObjectKey: "stackr/org1/a.tar.gz", SizeBytes: 10, CreatedAt: now}
	require.NoError(t, s.CreateBackupRun(ctx, runA), "create run")
	return twoOrgs{seed: seed, otherOrg: org2, otherDB: db2,
		destA: destA, destB: destB, backupA: backupA, runA: runA, backupB: backupB}
}

func wantStatus(t *testing.T, err error, code int, what string) {
	t.Helper()
	var he *echo.HTTPError
	require.ErrorAs(t, err, &he, "%s: want %d, got %v", what, code, err)
	require.Equal(t, code, he.Code, "%s: want %d, got %v", what, code, err)
}

// A member of org2 must not reach org1's backup through any route, even
// holding every backup scope.
func TestBackupRoutesRejectOtherOrg(t *testing.T) {
	s := testdb.New(t)
	fx := seedTwoOrgs(t, s)
	a := apiFor(s)
	outsider := []string{fx.otherOrg.ID}

	routes := []struct {
		name string
		h    echo.HandlerFunc
		verb string
		body string
		scp  string
		v    service.Verb
	}{
		{"list runs", a.listBackupRuns, http.MethodGet, "", ScopeBackupsRead, service.VerbOrgRead},
		{"run now", a.runBackup, http.MethodPost, "", ScopeBackupsWrite, service.VerbBackupWrite},
		{"update", a.patchBackup, http.MethodPatch, `{"cron":"0 5 * * *"}`, ScopeBackupsWrite, service.VerbBackupWrite},
		{"delete", a.deleteBackup, http.MethodDelete, "", ScopeBackupsWrite, service.VerbBackupWrite},
		{"restore", a.restoreBackup, http.MethodPost, `{"run_id":"runA"}`, ScopeBackupsRestore, service.VerbBackupWrite},
	}
	for _, r := range routes {
		t.Run(r.name, func(t *testing.T) {
			_, err := callGated(t, a, r.h, r.verb, "/", r.body, fx.backupA.ID, outsider,
				r.v, service.KindBackup, r.scp)
			wantStatus(t, err, http.StatusNotFound, r.name+" on another org's backup")
		})
	}
	// And the tile-scoped listing/creation must not open either.
	_, err := callGated(t, a, a.listBackups, http.MethodGet, "/", "", fx.seed.Tile.ID, outsider,
		service.VerbTileRead, service.KindTile, ScopeBackupsRead)
	wantStatus(t, err, http.StatusNotFound, "list another org's tile backups")
}

// The tile is the caller's; the destination is not. A route-level tile→org
// check passes this cleanly, which is why the destination is checked
// separately, it carries the bucket credentials the archive is written with.
func TestCreateBackupRejectsOtherOrgDestination(t *testing.T) {
	s := testdb.New(t)
	fx := seedTwoOrgs(t, s)
	a := apiFor(s)

	// org1's tile, pointed at org2's destination.
	body := `{"destination_id":"` + fx.destB.ID + `","kind":"volume","cron":"0 3 * * *"}`
	_, err := call(t, a, a.createBackup, http.MethodPost, "/", body, fx.seed.Tile.ID, ScopeBackupsWrite)
	wantStatus(t, err, http.StatusNotFound, "create with another org's destination")
}

// Restore names an archive by run id. If the run's backup were not checked,
// a run id from another org's schedule would restore that org's archive over
// this tile, and read the object out of their bucket to do it.
func TestRestoreRejectsRunFromAnotherBackup(t *testing.T) {
	s := testdb.New(t)
	fx := seedTwoOrgs(t, s)
	a := apiFor(s)
	ctx := context.Background()

	// A run that belongs to org2's schedule.
	foreign := &repo.BackupRun{ID: "runB", BackupID: fx.backupB.ID, Trigger: "manual", Status: "done",
		ObjectKey: "stackr/org2/secret.dump.gz", SizeBytes: 10, CreatedAt: time.Now().UTC()}
	require.NoError(t, s.CreateBackupRun(ctx, foreign), "create foreign run")
	_, err := call(t, a, a.restoreBackup, http.MethodPost, "/", `{"run_id":"runB"}`, fx.backupA.ID, ScopeBackupsRestore)
	require.Error(t, err, "restoring another backup's run should fail")
	wantStatus(t, err, http.StatusNotFound, "restore a run from another backup")
}

// Destinations are listed per caller: their own orgs', plus a server-wide one
// only once an admin has shared it. A destination carries the credentials the
// archive is written with, so an unshared one is simply absent.
func TestListDestinationsFiltersByOrg(t *testing.T) {
	s := testdb.New(t)
	fx := seedTwoOrgs(t, s)
	a := apiFor(s)
	ctx := context.Background()

	global := &repo.BackupDestination{ID: "destG", Name: "global", Endpoint: "https://s3", Bucket: "g", CreatedAt: time.Now().UTC()}
	require.NoError(t, s.CreateBackupDestination(ctx, global), "create global")

	list := func() map[string]bool {
		t.Helper()
		rec, err := callAs(t, a, a.listDestinations, http.MethodGet, "/", "", "", []string{fx.otherOrg.ID}, ScopeBackupsRead)
		require.NoError(t, err, "list")
		var out []destinationOut
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &out), "decode %s", rec.Body.String())
		seen := map[string]bool{}
		for _, d := range out {
			seen[d.ID] = true
		}
		return seen
	}

	seen := list()
	assert.False(t, seen[fx.destA.ID], "another org's destination was listed")
	assert.True(t, seen[fx.destB.ID], "own destination should be listed")
	assert.False(t, seen["destG"], "an unshared server-wide destination must not be offered")

	global.Shared = true
	require.NoError(t, s.UpdateBackupDestination(ctx, global), "share it")
	assert.True(t, list()["destG"], "a shared server-wide destination should be listed")
}
