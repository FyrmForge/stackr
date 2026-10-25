package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/internal/secrets"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

var nextKey = strings.Repeat("cd", 32)

// keyBox is a data dir with keys/master.key and a database sealed under it:
// one sealed param plus the canary.
type keyBox struct {
	dir string
	st  *store.Store
}

func newKeyBox(t *testing.T) keyBox {
	t.Helper()
	dir := t.TempDir()
	must(t, installspec.WriteKeyFile(filepath.Join(dir, installspec.KeyFile), []byte(testKey+"\n")))
	db, err := openDB(context.Background(), filepath.Join(dir, "stackr.db"))
	must(t, err)
	t.Cleanup(func() { _ = db.Close() })
	box, err := secrets.New(testKey)
	must(t, err)
	st := store.New(db, box)
	ctx := context.Background()
	must(t, ensureCanary(ctx, st))
	now := time.Now()
	must(t, st.Params.Create(ctx, store.Param{ID: "p1", ScopeKind: "server", Name: "K", Kind: "secret", Value: "s3cret", CreatedAt: now, UpdatedAt: now}))
	return keyBox{dir, st}
}

func (k keyBox) writeNext(t *testing.T) {
	t.Helper()
	must(t, installspec.WriteKeyFile(filepath.Join(k.dir, installspec.KeyNextFile), []byte(nextKey+"\n")))
}

func (k keyBox) onDisk(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(k.dir, name))
	if os.IsNotExist(err) {
		return ""
	}
	must(t, err)
	return strings.TrimSpace(string(b))
}

// paramUnder reads the sealed param with key.
func (k keyBox) paramUnder(t *testing.T, key string) (string, error) {
	t.Helper()
	box, err := secrets.New(key)
	must(t, err)
	p, err := store.New(k.st.DB(), box).Params.Get(context.Background(), "p1")
	return p.Value, err
}

// A rotation waiting at boot re-seals everything, then the next key replaces
// the current one and the generation counts up.
func TestLoadMasterKeyFinishesRotation(t *testing.T) {
	k := newKeyBox(t)
	k.writeNext(t)
	got, err := LoadMasterKey(context.Background(), k.dir, "", "")
	must(t, err)
	if got != nextKey || k.onDisk(t, installspec.KeyFile) != nextKey || k.onDisk(t, installspec.KeyNextFile) != "" {
		t.Fatalf("key = current %v, next file left %q", got == nextKey, k.onDisk(t, installspec.KeyNextFile))
	}
	if v, err := k.paramUnder(t, nextKey); err != nil || v != "s3cret" {
		t.Errorf("param under the new key = %q, %v", v, err)
	}
	if gen, _ := MasterGen(k.dir); gen != 2 {
		t.Errorf("generation = %d, want 2", gen)
	}
}

// A crash after the transaction committed but before the rename leaves the
// database on the next key; the canary says so and the boot finishes the rename.
func TestLoadMasterKeyCrashAfterTransaction(t *testing.T) {
	k := newKeyBox(t)
	k.writeNext(t)
	cur, _ := secrets.New(testKey)
	next, _ := secrets.New(nextKey)
	must(t, k.st.Reseal(context.Background(), cur, next))

	got, err := LoadMasterKey(context.Background(), k.dir, "", "")
	must(t, err)
	if got != nextKey || k.onDisk(t, installspec.KeyFile) != nextKey || k.onDisk(t, installspec.KeyNextFile) != "" {
		t.Fatalf("booted on the next key = %v", got == nextKey)
	}
	if v, err := k.paramUnder(t, nextKey); err != nil || v != "s3cret" {
		t.Errorf("param under the new key = %q, %v", v, err)
	}
}

// A transaction that fails rolls back whole: the boot stays on the old key,
// the unused next key is dropped, nothing in the database moved.
func TestLoadMasterKeyFailedTransactionKeepsOldKey(t *testing.T) {
	k := newKeyBox(t)
	k.writeNext(t)
	_, err := k.st.DB().Exec(`INSERT INTO params (id, scope_kind, scope_id, collection, name, kind, value, created_at, updated_at)
		VALUES ('bad', 'server', '', '', 'B', 'secret', 'enc1:garbage', ?, ?)`, time.Now(), time.Now())
	must(t, err)

	got, err := LoadMasterKey(context.Background(), k.dir, "", "")
	must(t, err)
	if got != testKey || k.onDisk(t, installspec.KeyFile) != testKey || k.onDisk(t, installspec.KeyNextFile) != "" {
		t.Fatalf("stayed on the old key = %v", got == testKey)
	}
	if v, err := k.paramUnder(t, testKey); err != nil || v != "s3cret" {
		t.Errorf("param under the old key = %q, %v", v, err)
	}
}

// The env key is the operator's: a leftover next file is ignored, and the
// panel refuses to rotate it.
func TestRotateMasterKeyRefusedOnEnvKey(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	admin, err := w.orch.users.CreateAdmin(ctx, "adm@x.test", "correct horse battery", "Adm")
	must(t, err)
	if err := w.orch.RotateMasterKey(ctx, admin.ID, "wrong"); err == nil {
		t.Fatal("rotated behind a wrong password")
	}
	w.orch.cfg.KeyFromEnv = true
	if err := w.orch.RotateMasterKey(ctx, admin.ID, "correct horse battery"); err == nil {
		t.Fatal("rotated an environment key")
	}
	if _, err := os.Stat(filepath.Join(w.orch.cfg.DataDir, installspec.KeyNextFile)); err == nil {
		t.Fatal("a next key was written")
	}
	w.orch.cfg.KeyFromEnv = false
	must(t, w.orch.RotateMasterKey(ctx, admin.ID, "correct horse battery"))
	if _, err := secrets.New(readFile(t, w.orch.cfg.DataDir, installspec.KeyNextFile)); err != nil {
		t.Errorf("next key: %v", err)
	}
	if !w.orch.MasterKeyStatus().Pending {
		t.Error("status does not show the waiting rotation")
	}
}
