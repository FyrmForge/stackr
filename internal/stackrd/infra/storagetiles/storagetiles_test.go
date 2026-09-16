package storagetiles

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/agent"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/cluster"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

func TestParseAttachment(t *testing.T) {
	slug, name, mount, ro, err := ParseAttachment("nas-media/tv:/tv")
	require.NoError(t, err)
	assert.True(t, slug == "nas-media" && name == "tv" && mount == "/tv" && !ro)

	slug, name, mount, ro, err = ParseAttachment("ssd-pool/configs:/config:ro")
	require.NoError(t, err)
	assert.True(t, slug == "ssd-pool" && name == "configs" && mount == "/config" && ro)

	// Only an org share line may skip the path name: it mounts the root.
	slug, name, mount, _, err = ParseAttachment("${{ org.storage.media }}:/media")
	require.NoError(t, err)
	assert.True(t, slug == "${{ org.storage.media }}" && name == "" && mount == "/media")

	for _, bad := range []string{"tv:/tv", "nas/", "nas/tv:relative", "nas/tv", ":/x", "/tv:/tv"} {
		_, _, _, _, err := ParseAttachment(bad)
		assert.Error(t, err, bad)
	}
}

func TestVolumeOpts(t *testing.T) {
	nfs := &repo.Storage{Slug: "nas", Backend: "nfs", Address: "192.168.1.50", Export: "/export/media"}
	o, err := VolumeOpts(nfs, "tv")
	require.NoError(t, err)
	assert.Equal(t, "nfs", o["type"])
	assert.Equal(t, ":/export/media/tv", o["device"])
	assert.Contains(t, o["o"], "addr=192.168.1.50")

	smb := &repo.Storage{Slug: "nas2", Backend: "smb", Address: "192.168.1.51", Export: "share", Username: "u", Password: "p"}
	o, err = VolumeOpts(smb, "media/tv")
	require.NoError(t, err)
	assert.Equal(t, "cifs", o["type"])
	assert.Equal(t, "//192.168.1.51/share/media/tv", o["device"])
	assert.Contains(t, o["o"], "username=u")

	local := &repo.Storage{Slug: "ssd", Backend: "local", Export: "/mnt/ssd/pool"}
	o, err = VolumeOpts(local, "grafana")
	require.NoError(t, err)
	assert.Equal(t, "none", o["type"])
	assert.Equal(t, "bind", o["o"])
	assert.Equal(t, "/mnt/ssd/pool/grafana", o["device"])

	_, err = VolumeOpts(&repo.Storage{Backend: "local", Export: "relative"}, "")
	require.Error(t, err, "local pool with a relative path must fail")
}

func TestValidateAttach(t *testing.T) {
	db := &repo.Tile{Engine: "postgres"}
	svc := &repo.Tile{Kind: "service"}
	nfs := &repo.Storage{Name: "nas", Backend: "nfs"}
	local := &repo.Storage{Name: "ssd", Backend: "local"}
	assert.Error(t, ValidateAttach(nfs, db), "network storage on a managed instance must be refused")
	assert.NoError(t, ValidateAttach(local, db))
	assert.NoError(t, ValidateAttach(nfs, svc))
}

// A local pool is a directory on one host. Docker auto-creates an unknown
// named volume as an empty local one, so attaching a worker's pool to a tile
// that runs on the manager mounted an empty directory and reported the task
// healthy. The refusal has to name both sides.
func TestLocalPoolOnAnotherMachineIsRefusedNotSilentlyEmpty(t *testing.T) {
	ctx := context.Background()
	store := testdb.New(t)
	seed := testdb.SeedStack(t, store, false)
	c := cluster.New(nil, agent.New(nil, "", t.TempDir()), store)
	now := time.Now().UTC()

	require.NoError(t, store.CreateServer(ctx, &repo.Server{ID: "srv-worker",
		Name: "worker-2", Kind: "swarm", NodeID: "node-worker", CreatedAt: now}))
	st := &repo.Storage{ID: "st-pool", ServerID: "srv-worker", Name: "QA pool",
		Slug: "qa-pool", Backend: "local", Export: "/srv/qa-pool", CreatedAt: now}
	require.NoError(t, store.CreateStorage(ctx, st))
	require.NoError(t, store.CreateStoragePath(ctx, &repo.StoragePath{ID: "sp-media",
		StorageID: st.ID, Name: "media", Subpath: "media", CreatedAt: now}))

	// A pinned tile whose home node is not the pool's machine.
	elsewhere := &repo.Tile{ID: "pinned-db", StackID: seed.Stack.ID, EnvironmentID: seed.Env.ID,
		Name: "sharedpg", Slug: "sharedpg", Kind: "database", Engine: "postgres",
		HomeNode: "node-manager", Status: "running", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, store.CreateTile(ctx, elsewhere))

	_, err := Resolve(ctx, c, store, elsewhere, "qa-pool/media:/data")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "qa-pool")
	assert.Contains(t, err.Error(), "sharedpg")
	assert.Contains(t, err.Error(), "empty directory")
}
