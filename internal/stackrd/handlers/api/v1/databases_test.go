package v1

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo/sqlite"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

// A shared instance's published port, resource limits and sharing scope were
// settable only from the canvas, the API could create an instance it could
// never configure. These pin the guards on the route that closes that.

func seedDB(t *testing.T, s *sqlite.Store, seed testdb.Seed) *repo.Tile {
	t.Helper()
	now := time.Now().UTC()
	d := &repo.Tile{ID: "db1", StackID: seed.Stack.ID, EnvironmentID: seed.Env.ID,
		Name: "pg", Slug: "pg", Kind: "service", Engine: "postgres", SourceType: "image",
		Status: "idle", ExternalPort: 15432, CreatedAt: now, UpdatedAt: now}
	require.NoError(t, s.CreateTile(context.Background(), d), "create db tile")
	return d
}

func TestPatchDBUpdatesSettings(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	d := seedDB(t, s, seed)
	a := apiFor(s)

	body := `{"external_port":25432,"scope":"org","cpu_limit":1.5,"mem_limit_mb":3}`
	rec, err := call(t, a, a.patchDB, http.MethodPatch, "/", body, d.ID, ScopeDBsWrite)
	require.NoError(t, err, "patch")
	var got dbOut
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &got), "decode")
	assert.True(t, got.ExternalPort == 25432 && got.Scope == "org" && got.CPULimit == 1.5,
		"response = %+v", got)
	// Docker rejects memory caps under 6MB, so 3 is raised rather than saved.
	assert.Equal(t, 6, got.MemLimitMB, "mem_limit_mb, want the 6MB floor")
	stored, _ := s.GetTile(context.Background(), d.ID)
	assert.True(t, stored.ScopeKind == "org" && stored.ScopeID == seed.Org.ID,
		"scope stored as %s/%s, want org/%s", stored.ScopeKind, stored.ScopeID, seed.Org.ID)
}

// Omitted fields must survive: a patch that only moves the port must not reset
// the limits to zero.
func TestPatchDBLeavesOmittedFields(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	d := seedDB(t, s, seed)
	d.CPULimit, d.MemLimitMB = 2, 512
	require.NoError(t, s.UpdateTile(context.Background(), d.ID, d.TileConfig), "seed limits")
	a := apiFor(s)

	_, err := call(t, a, a.patchDB, http.MethodPatch, "/", `{"external_port":0}`, d.ID, ScopeDBsWrite)
	require.NoError(t, err, "patch")
	stored, _ := s.GetTile(context.Background(), d.ID)
	assert.True(t, stored.ExternalPort == 0 && stored.CPULimit == 2 && stored.MemLimitMB == 512,
		"stored = port %d cpu %v mem %d, want the limits untouched",
		stored.ExternalPort, stored.CPULimit, stored.MemLimitMB)
}

func TestPatchDBRejectsBadInput(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	d := seedDB(t, s, seed)
	a := apiFor(s)

	for _, tc := range []struct{ name, body string }{
		{"port out of range", `{"external_port":70000}`},
		{"negative port", `{"external_port":-1}`},
		{"unknown scope", `{"scope":"server"}`},
		{"negative cpu", `{"cpu_limit":-1}`},
	} {
		_, err := call(t, a, a.patchDB, http.MethodPatch, "/", tc.body, d.ID, ScopeDBsWrite)
		wantStatus(t, err, http.StatusBadRequest, tc.name)
	}

	// A service tile is not addressable through the db routes.
	_, err := call(t, a, a.patchDB, http.MethodPatch, "/", `{"external_port":1}`, seed.Tile.ID, ScopeDBsWrite)
	wantStatus(t, err, http.StatusNotFound, "patching a service tile as a db")
}

// All three fields are config-modeled now, so on a file-owned stack the API
// must refuse rather than write drift the next apply reverts.
func TestPatchDBRejectsManagedStack(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, true)
	d := seedDB(t, s, seed)
	a := apiFor(s)

	_, err := call(t, a, a.patchDB, http.MethodPatch, "/", `{"external_port":25432}`, d.ID, ScopeDBsWrite)
	wantStatus(t, err, http.StatusConflict, "patching a db on a config-managed stack")
}
