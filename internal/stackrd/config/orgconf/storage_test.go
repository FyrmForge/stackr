package orgconf

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
	"github.com/FyrmForge/stackr/internal/stackrd/store/testdb"
)

func TestParseStorage(t *testing.T) {
	f, err := Parse([]byte(`
version: 1
org: x
storage:
  media:
    backend: smb
    address: nas.lan
    export: media
    username: ${{ org.secrets.SMB_USER }}
    password: ${{ org.secrets.SMB_PASSWORD }}
    path: /library
`))
	require.NoError(t, err)
	st, missing := f.Storage["media"].want([]repo.Variable{{Name: "SMB_USER", Value: "u", Secret: true}})
	assert.Equal(t, []string{"SMB_PASSWORD"}, missing)
	assert.Equal(t, "u", st.Username)
	assert.Equal(t, "media/library", st.Export)

	bad := map[string]string{
		"local":      "storage:\n  m: {backend: local, address: a, export: /x}\n",
		"no export":  "storage:\n  m: {backend: nfs, address: a}\n",
		"bad name":   "storage:\n  Media: {backend: nfs, address: a, export: /x}\n",
		"stack cred": "storage:\n  m: {backend: smb, address: a, export: s, password: '${{ stack.secrets.P }}'}\n",
	}
	for name, y := range bad {
		_, err := Parse([]byte("version: 1\norg: x\n" + y))
		assert.Error(t, err, name)
	}
}

func TestDiffStorage(t *testing.T) {
	s := testdb.New(t)
	seed := testdb.SeedStack(t, s, false)
	r := Runner{Store: s}
	ctx := context.Background()

	f, err := Parse([]byte("version: 1\norg: " + seed.Org.Slug + "\nstorage:\n  media: {backend: nfs, address: nas, export: /srv}\n"))
	require.NoError(t, err)
	p, err := r.diff(ctx, seed.Org, f)
	require.NoError(t, err)
	require.Len(t, p.Changes, 1)
	assert.Equal(t, "create", p.Changes[0].Kind)
	assert.Equal(t, "media", p.Changes[0].New)

	// A share a tile still mounts cannot leave the file.
	require.NoError(t, s.CreateStorage(ctx, &repo.Storage{ID: "0123456789ab", OrgID: seed.Org.ID, Name: "old", Slug: "old",
		Backend: "nfs", Address: "nas", Export: "/srv", CreatedAt: time.Now()}))
	seed.Tile.Storage = "${{ org.storage.old }}/tv:/tv"
	require.NoError(t, s.UpdateTile(ctx, seed.Tile.ID, seed.Tile.TileConfig))
	p, err = r.diff(ctx, seed.Org, f)
	require.NoError(t, err)
	require.Len(t, p.Errors, 1)
	assert.Contains(t, p.Errors[0], "still mounted")
}
