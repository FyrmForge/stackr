package service

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"filippo.io/age"

	"github.com/FyrmForge/stackr/internal/installspec"
)

// opensWith reports whether the panel archive of run r opens with key.
func (w *world) opensWith(t *testing.T, objectKey, key string) bool {
	t.Helper()
	local, err := w.orch.localDest(context.Background())
	must(t, err)
	f, err := os.Open(filepath.Join(local.Endpoint, objectKey))
	must(t, err)
	defer func() { _ = f.Close() }()
	id, err := age.ParseX25519Identity(key)
	must(t, err)
	_, err = age.Decrypt(f, id)
	return err == nil
}

// A box with no recovery key makes the pair at boot: the public half and
// generation 1 on disk, the private half pending until it is saved. A panel
// backup opens only with that private half, shows the generation, and the box
// never keeps the private half once it is confirmed.
func TestRecoveryKeyBootRevealConfirm(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	admin, err := w.orch.users.CreateAdmin(ctx, "adm@x.test", "correct horse battery", "Adm")
	must(t, err)

	pub, gen, err := installspec.LoadRecovery(w.orch.cfg.DataDir)
	if err != nil || pub == "" || gen != 1 {
		t.Fatalf("recovery.pub = %q gen %d, %v", pub, gen, err)
	}
	if g, unsaved := w.orch.RecoveryStatus(); g != 1 || !unsaved {
		t.Fatalf("status = gen %d unsaved %v", g, unsaved)
	}
	if _, err := w.orch.RevealRecoveryKey(ctx, admin.ID, "not the password"); err == nil {
		t.Fatal("the key was shown behind a wrong password")
	}
	key, err := w.orch.RevealRecoveryKey(ctx, admin.ID, "correct horse battery")
	must(t, err)
	if strings.Contains(readFile(t, w.orch.cfg.DataDir, installspec.RecoveryPubFile), "AGE-SECRET-KEY") {
		t.Error("the private half is in recovery.pub")
	}

	r, err := w.orch.panelBackup(ctx, io.Discard, "manual", "")
	must(t, err)
	if r.RecoveryGen != 1 || !w.opensWith(t, r.ObjectKey, key) {
		t.Errorf("run gen %d, opens with the panel-made key = %v", r.RecoveryGen, w.opensWith(t, r.ObjectKey, key))
	}
	if r.MasterGen != 1 {
		t.Errorf("run master gen = %d, want 1", r.MasterGen)
	}

	must(t, w.orch.ConfirmRecoveryKey())
	if _, unsaved := w.orch.RecoveryStatus(); unsaved {
		t.Error("still unsaved after confirm")
	}
	if _, err := os.Stat(filepath.Join(w.orch.cfg.DataDir, installspec.RecoveryPendingFile)); err == nil {
		t.Error("the private half is still on the box after confirm")
	}
	if _, err := w.orch.RevealRecoveryKey(ctx, admin.ID, "correct horse battery"); err == nil {
		t.Error("a saved key was shown again")
	}
}

// Rotation (behind the password, and only once the current key is saved)
// moves to generation 2: new archives need the new key, old ones keep the old.
func TestRotateRecoveryKey(t *testing.T) {
	w := newWorld(t)
	ctx := context.Background()
	admin, err := w.orch.users.CreateAdmin(ctx, "adm@x.test", "correct horse battery", "Adm")
	must(t, err)
	oldKey, err := w.orch.RevealRecoveryKey(ctx, admin.ID, "correct horse battery")
	must(t, err)
	old, err := w.orch.panelBackup(ctx, io.Discard, "manual", "")
	must(t, err)

	if _, err := w.orch.RotateRecoveryKey(ctx, admin.ID, "correct horse battery"); err == nil {
		t.Fatal("rotated while the current key was unsaved")
	}
	must(t, w.orch.ConfirmRecoveryKey())
	if _, err := w.orch.RotateRecoveryKey(ctx, admin.ID, "wrong"); err == nil {
		t.Fatal("rotated behind a wrong password")
	}
	newKey, err := w.orch.RotateRecoveryKey(ctx, admin.ID, "correct horse battery")
	must(t, err)
	if g, unsaved := w.orch.RecoveryStatus(); g != 2 || !unsaved {
		t.Errorf("after rotation: gen %d unsaved %v", g, unsaved)
	}
	if _, g, _ := installspec.LoadRecovery(w.orch.cfg.DataDir); g != 2 {
		t.Errorf("recovery.pub gen = %d", g)
	}
	fresh, err := w.orch.panelBackup(ctx, io.Discard, "manual", "")
	must(t, err)
	if fresh.RecoveryGen != 2 || !w.opensWith(t, fresh.ObjectKey, newKey) || w.opensWith(t, fresh.ObjectKey, oldKey) {
		t.Errorf("new archive: gen %d, opens with new %v, with old %v", fresh.RecoveryGen,
			w.opensWith(t, fresh.ObjectKey, newKey), w.opensWith(t, fresh.ObjectKey, oldKey))
	}
	if !w.opensWith(t, old.ObjectKey, oldKey) || w.opensWith(t, old.ObjectKey, newKey) {
		t.Error("the archive made before the rotation no longer needs the old key")
	}
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, name))
	must(t, err)
	return string(b)
}
