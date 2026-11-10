package worker

import (
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
)

// Key rotation. The node makes new mesh and exit keys itself (the private
// halves never leave it), switches to them, and tells the controller the new
// public keys through the recovery path: its cert, not its WireGuard key,
// proves who it is, so the call works while every tunnel is down. The old
// keys stay as *.prev until the controller has taken the new ones; a node
// that cannot reach the panel for RotateRevertAfter goes back to them.
var (
	RotateRevertAfter = 5 * time.Minute
	rotateRetry       = 10 * time.Second
	rotateGrace       = time.Second // the 204 leaves over the old key first
)

// RotatePath is POST: rotate this node's keys (answers 204 at once).
const RotatePath = "/v1/mesh/rotate"

func (s *Server) postRotate(w http.ResponseWriter, _ *http.Request) {
	w.WriteHeader(http.StatusNoContent)
	go s.rotate()
}

// rotate swaps the keys, then recovers until the controller has them.
func (s *Server) rotate() {
	time.Sleep(rotateGrace)
	if err := s.swapKeys(); err != nil {
		slog.Error("worker: rotate mesh keys", "err", err)
		return
	}
	s.recoverOrRevert(RotateRevertAfter, rotateRetry)
}

// recoverOrRevert calls the panel with the keys on disk until it takes them
// (the *.prev keys go), or reverts to *.prev after RotateRevertAfter.
func (s *Server) recoverOrRevert(revertAfter, retry time.Duration) {
	deadline := time.Now().Add(revertAfter)
	for {
		err := s.recoverOnce(s.ctx)
		if err == nil {
			s.dropPrev()
			slog.Info("worker: rotated its mesh keys")
			return
		}
		if time.Now().After(deadline) {
			slog.Warn("worker: the panel never took the new keys; back to the old ones", "err", err)
			if err := s.revertKeys(); err != nil {
				slog.Error("worker: revert mesh keys", "err", err)
			}
			_ = s.recoverOnce(s.ctx)
			return
		}
		select {
		case <-s.ctx.Done():
			return
		case <-time.After(retry):
		}
	}
}

func (s *Server) keyPaths() []string { return []string{s.wgKeyPath(), s.exitKeyPath()} }

// swapKeys keeps the current keys as *.prev, writes new ones and re-applies
// the last spec with them.
func (s *Server) swapKeys() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.keyPaths() {
		if _, _, err := loadKey(p); err != nil { // makes one that is missing
			return err
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		if err := writeKey(p+".prev", b); err != nil {
			return err
		}
		k, err := wgtypes.GeneratePrivateKey()
		if err != nil {
			return err
		}
		if err := writeKey(p, []byte(k.String()+"\n")); err != nil {
			return err
		}
	}
	return s.reapply()
}

// revertKeys puts the *.prev keys back and re-applies.
func (s *Server) revertKeys() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.keyPaths() {
		b, err := os.ReadFile(p + ".prev")
		if err != nil {
			return err
		}
		if err := writeKey(p, b); err != nil {
			return err
		}
	}
	if err := s.reapply(); err != nil {
		return err
	}
	s.dropPrev() // back on the old keys: no rotation is pending any more
	return nil
}

// ResumeRotation is the start-up half of a rotation: a *.prev key left
// behind means a restart cut it short, after the swap. The new keys stay (the
// panel may already hold them) and the same recover-or-revert runs again, so
// the node goes back to its old keys only when the panel never takes the new
// ones. Only a worker's Run calls it: the controller's own *.prev is the
// panel's to manage. Reports whether a rotation was resumed.
func (s *Server) ResumeRotation() bool {
	if _, err := os.Stat(s.wgKeyPath() + ".prev"); err != nil {
		return false
	}
	// the timings are read here, not in the goroutine, so nothing races a caller that changes them later
	revertAfter, retry := RotateRevertAfter, rotateRetry
	s.resuming.Add(1)
	go func() {
		defer s.resuming.Done()
		s.recoverOrRevert(revertAfter, retry)
	}()
	return true
}

func (s *Server) dropPrev() {
	for _, p := range s.keyPaths() {
		_ = os.Remove(p + ".prev")
	}
}

// reapply brings the interface and the exit links up again on the keys on
// disk: the links are rebuilt even though the spec did not change. s.mu is
// held.
func (s *Server) reapply() error {
	spec, err := s.savedMesh()
	if err != nil || spec == nil {
		return err // nothing applied yet: the next push uses the new keys
	}
	if s.mesh.applied != nil {
		cp := *s.mesh.applied
		cp.Links, cp.Serves = nil, nil
		s.mesh.applied = &cp
	}
	if err := s.meshNet(*spec); err != nil {
		return err
	}
	return s.meshExits(s.ctx, *spec)
}

// writeKey writes a private key file 0600, atomically.
func writeKey(path string, b []byte) error { return writeDurable(path, b, 0o600) }

// writeDurable writes temp, fsyncs it, renames, fsyncs the directory: after
// a power loss the old file or the new one is there, never an older state of
// a rename the caller already acted on.
func writeDurable(path string, b []byte, mode os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err = f.Write(b); err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

// StageKeys is the controller's own rotation, step one: new keys in
// *.next, not used yet, so the workers learn them before the switch.
func (s *Server) StageKeys() (wgPub, exitPub string, err error) {
	pubs := make([]string, 0, 2)
	for _, p := range s.keyPaths() {
		k, err := wgtypes.GeneratePrivateKey()
		if err != nil {
			return "", "", err
		}
		if err := writeKey(p+".next", []byte(k.String()+"\n")); err != nil {
			return "", "", err
		}
		pubs = append(pubs, k.PublicKey().String())
	}
	return pubs[0], pubs[1], nil
}

// PromoteKeys is step two: the staged keys replace the current ones (kept
// as *.prev) and the mesh is re-applied with them.
func (s *Server) PromoteKeys() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.keyPaths() {
		next, err := os.ReadFile(p + ".next")
		if err != nil {
			return fmt.Errorf("no staged key: %w", err)
		}
		if cur, err := os.ReadFile(p); err == nil {
			if err := writeKey(p+".prev", cur); err != nil {
				return err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		if err := writeKey(p, next); err != nil {
			return err
		}
		_ = os.Remove(p + ".next")
	}
	return s.reapply()
}

// WGPublicKey is the node's current WireGuard public key.
func (s *Server) WGPublicKey() string {
	wg, _ := s.publicKeys()
	return wg
}

// publicKeys are the node's current public keys, for a recovery call.
func (s *Server) publicKeys() (wg, exit string) {
	_, wg, _ = loadKey(s.wgKeyPath())
	_, exit, _ = loadKey(s.exitKeyPath())
	return wg, exit
}
