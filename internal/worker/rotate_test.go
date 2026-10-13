package worker

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/FyrmForge/stackr/internal/exits"
	"github.com/FyrmForge/stackr/internal/wgapply"
)

// pubOf is the public key of the private key the interface was last given.
func pubOf(t *testing.T, rec *wgapply.Recorder) string {
	t.Helper()
	cfg, ok := rec.Last()
	if !ok {
		t.Fatal("nothing applied")
	}
	k, err := wgtypes.ParseKey(cfg.PrivateKey)
	if err != nil {
		t.Fatal(err)
	}
	return k.PublicKey().String()
}

func linkUps(run *meshRunner) int {
	run.mu.Lock()
	defer run.mu.Unlock()
	n := 0
	for _, c := range run.calls {
		if strings.HasPrefix(c, "ip link add "+exits.Iface("nx")) {
			n++
		}
	}
	return n
}

// A rotation puts new keys on the interface and the exit links, keeps the
// old ones as *.prev, and the next recovery call carries the new public keys;
// a revert puts the old ones back.
func TestRotateSwapsAndReverts(t *testing.T) {
	dir := t.TempDir()
	s, run, rec := newMeshServer(t, dir, &pingFake{})
	ctx := context.Background()
	if err := s.ApplyMesh(ctx, meshSpec()); err != nil {
		t.Fatal(err)
	}
	old, ups := pubOf(t, rec), linkUps(run)
	oldExit := s.mustInfo(t).ExitPublic

	if err := s.swapKeys(); err != nil {
		t.Fatal(err)
	}
	if pubOf(t, rec) == old {
		t.Fatal("the interface kept the old key")
	}
	if linkUps(run) != ups+1 {
		t.Fatal("the exit link was not rebuilt on the new key")
	}
	if s.mustInfo(t).ExitPublic == oldExit {
		t.Fatal("the exit key did not change")
	}
	for _, p := range s.keyPaths() {
		if fi, err := os.Stat(p + ".prev"); err != nil || fi.Mode().Perm() != 0o600 {
			t.Fatalf("%s.prev: %v", p, err)
		}
	}

	var got RecoverPayload
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req RecoverRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		_, got, _ = OpenRecover(req)
		_ = json.NewEncoder(w).Encode(meshSpec())
	}))
	defer panel.Close()
	s.cfg.Recover = RecoverConfig{Own: selfCert(t), Panel: panel.URL}
	if err := s.recoverOnce(ctx); err != nil {
		t.Fatal(err)
	}
	if got.WGPublic != pubOf(t, rec) || got.ExitPublic != s.mustInfo(t).ExitPublic {
		t.Fatal("the recovery call did not carry the new public keys")
	}

	if err := s.revertKeys(); err != nil {
		t.Fatal(err)
	}
	if pubOf(t, rec) != old || s.mustInfo(t).ExitPublic != oldExit {
		t.Fatal("revert did not restore the old keys")
	}
}

// The controller's own rotation: staged keys are not used until promoted.
func TestStageThenPromote(t *testing.T) {
	s, _, rec := newMeshServer(t, t.TempDir(), &pingFake{})
	if err := s.ApplyMesh(context.Background(), meshSpec()); err != nil {
		t.Fatal(err)
	}
	old := pubOf(t, rec)
	wg, exit, err := s.StageKeys()
	if err != nil {
		t.Fatal(err)
	}
	if wg == old || s.mustInfo(t).WGPublic != old {
		t.Fatal("staging switched the key early")
	}
	if err := s.PromoteKeys(); err != nil {
		t.Fatal(err)
	}
	if pubOf(t, rec) != wg || s.mustInfo(t).ExitPublic != exit {
		t.Fatal("promote did not switch to the staged keys")
	}
}

// A panel that never takes the new keys: the node goes back to the old ones.
func TestRotateRevertsWhenPanelRefuses(t *testing.T) {
	ra, rr, rg := RotateRevertAfter, rotateRetry, rotateGrace
	defer func() { RotateRevertAfter, rotateRetry, rotateGrace = ra, rr, rg }()
	RotateRevertAfter, rotateRetry, rotateGrace = 0, 0, 0
	panel := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "no", 403) }))
	defer panel.Close()
	s, _, rec := newMeshServer(t, t.TempDir(), &pingFake{})
	if err := s.ApplyMesh(context.Background(), meshSpec()); err != nil {
		t.Fatal(err)
	}
	old := pubOf(t, rec)
	s.cfg.Recover = RecoverConfig{Own: selfCert(t), Panel: panel.URL}
	s.rotate()
	if pubOf(t, rec) != old {
		t.Fatal("kept new keys the controller never learned")
	}
}

func (s *Server) mustInfo(t *testing.T) MeshInfo {
	t.Helper()
	info, err := s.MeshInfo()
	if err != nil {
		t.Fatal(err)
	}
	return info
}
