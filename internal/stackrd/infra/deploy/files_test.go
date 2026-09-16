package deploy

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// A folder entry ships the whole tree into a per-deploy folder, and the
// prune after a converged deploy keeps only that folder.
func TestMaterializeFilesFolder(t *testing.T) {
	dataDir := t.TempDir()
	app := &repo.Tile{ID: "tile-1", SourceType: "git", Files: "config:/etc/app"}
	repoDir := filepath.Join(dataDir, "repos", app.ID)
	require.NoError(t, os.MkdirAll(filepath.Join(repoDir, "config", "dashboards"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "config", "app.yml"), []byte("a: 1"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(repoDir, "config", "dashboards", "one.json"), []byte("{}"), 0o644))
	require.NoError(t, os.Symlink("/etc/hostname", filepath.Join(repoDir, "config", "leak")))

	e := &Engine{dataDir: dataDir}
	binds, err := e.materializeFiles(context.Background(), nil, app, "deploy-2", io.Discard)
	require.NoError(t, err)
	dst := filepath.Join(dataDir, "files", app.ID, "deploy-2", "config")
	assert.Equal(t, []string{dst + ":/etc/app:ro"}, binds)
	b, err := os.ReadFile(filepath.Join(dst, "dashboards", "one.json"))
	require.NoError(t, err)
	assert.Equal(t, "{}", string(b))
	_, err = os.Lstat(filepath.Join(dst, "leak"))
	assert.True(t, os.IsNotExist(err), "symlinks are not shipped")

	old := filepath.Join(dataDir, "files", app.ID, "deploy-1")
	require.NoError(t, os.MkdirAll(old, 0o755))
	pruneFiles(dataDir, app.ID, "deploy-2")
	_, err = os.Stat(old)
	assert.True(t, os.IsNotExist(err))
	_, err = os.Stat(dst)
	assert.NoError(t, err)
}
