package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readBundle feeds the preview: it must carry every included file under the
// path exactly as the include: line spells it, or the server's fetcher misses.
func TestReadBundle(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.yml"),
		[]byte("version: 1\ninclude:\n  - inc/extra.yml\n"), 0o644))
	require.NoError(t, os.MkdirAll(filepath.Join(dir, "inc"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "inc", "extra.yml"), []byte("shared: {}\n"), 0o644))

	main, files, err := readBundle(filepath.Join(dir, "main.yml"))
	require.NoError(t, err)
	assert.Contains(t, main, "version: 1")
	require.Len(t, files, 1)
	assert.Equal(t, "shared: {}\n", files["inc/extra.yml"])
}

func TestReadBundleRejectsEscapes(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.yml"),
		[]byte("include:\n  - ../outside.yml\n"), 0o644))
	_, _, err := readBundle(filepath.Join(dir, "main.yml"))
	require.Error(t, err)
}
