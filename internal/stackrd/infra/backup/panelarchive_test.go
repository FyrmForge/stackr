package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	hamrsqlite "github.com/FyrmForge/hamr/pkg/db/sqlite"
	"github.com/stretchr/testify/require"

	"github.com/FyrmForge/stackr/internal/stackrd/config/secrets"
)

// A panel archive that ships the database without the key is the failure this
// whole shape exists to prevent: every secret in that database is ciphertext
// nothing else can read, and the backup reports success. So the test is about
// the members being present and the key round-tripping, not about the tar.
func TestPanelArchiveCarriesKeyAndVersion(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("STACKR_MASTER_KEY", "") // the file path branch, as an install uses
	require.NoError(t, secrets.Load(filepath.Join(dir, "keys", "master.key")))

	db, err := hamrsqlite.Connect(filepath.Join(dir, "stackr.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = db.Exec(`CREATE TABLE t (v TEXT)`)
	require.NoError(t, err)
	_, err = db.Exec(`INSERT INTO t VALUES (?)`, secrets.Encrypt("hunter2"))
	require.NoError(t, err)

	require.NoError(t, os.MkdirAll(filepath.Join(dir, "backups"), 0o700))
	s := &Service{db: db, dataDir: dir, version: "v1.2.3"}

	var buf bytes.Buffer
	require.NoError(t, s.writePanelArchive(&buf))

	members := map[string][]byte{}
	gz, err := gzip.NewReader(&buf)
	require.NoError(t, err)
	tr := tar.NewReader(gz)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		b, err := io.ReadAll(tr)
		require.NoError(t, err)
		members[h.Name] = b
	}

	require.Contains(t, members, "stackr.db")
	require.Contains(t, members, "keys/master.key")
	require.Contains(t, members, "VERSION")
	require.NotEmpty(t, members["stackr.db"])
	require.Equal(t, "v1.2.3", strings.TrimSpace(string(members["VERSION"])))

	// The key in the archive is the one that decrypts the database in it, and
	// it is in the form Load reads back, so the archive unpacks over a data
	// dir with nothing to convert.
	require.Equal(t, secrets.Key(), strings.TrimSpace(string(members["keys/master.key"])))
	onDisk, err := os.ReadFile(filepath.Join(dir, "keys", "master.key"))
	require.NoError(t, err)
	require.Equal(t, strings.TrimSpace(string(onDisk)), strings.TrimSpace(string(members["keys/master.key"])))
}
