package service

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/clusterca"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/worker"
)

// ackNode is a worker that records the controller serials it was told and
// which controller cert the panel was using at that moment.
type ackNode struct {
	fakeMeshNode
	o    *Orchestrator
	mu2  sync.Mutex
	seen [][]string
	cert []string
	fail bool
}

func (a *ackNode) ApplyMesh(_ context.Context, s worker.MeshSpec) error {
	a.mu2.Lock()
	defer a.mu2.Unlock()
	if a.fail {
		return errors.New("unreachable")
	}
	cur, _ := certSerial(a.o.cluster.cert())
	a.seen = append(a.seen, s.ControllerSerials)
	a.cert = append(a.cert, cur)
	return nil
}

// ctlRig is a panel with one worker row and a mesh that pushes to ackNode.
func ctlRig(t *testing.T) (*Orchestrator, *ackNode) {
	t.Helper()
	o := newClusterPanel(t)
	ctx := context.Background()
	ns, err := o.store.Nodes.List(ctx)
	must(t, err)
	ctl := ns[0]
	wk := ctl
	wk.ID, wk.Name, wk.Slug, wk.Role = "w1", "w1", "w1", "worker"
	wk.RangeCIDR, wk.WGAddress, wk.WGPublicKey = "10.214.0.0/16", "10.207.255.7", "wgkey-w1"
	must(t, o.store.Nodes.Create(ctx, wk))
	an := &ackNode{o: o}
	o.mesh = newMeshSvc(o.store, func(id string) (MeshNode, bool) {
		if id == "w1" {
			return an, true
		}
		return nil, false
	})
	o.mesh.ctlID, o.mesh.Trust = ctl.ID, o.meshTrust
	controllerAckEvery, controllerAckBound = 5*time.Millisecond, 50*time.Millisecond
	return o, an
}

func ctlSerial(t *testing.T, o *Orchestrator) string {
	t.Helper()
	s, err := certSerial(o.cluster.cert())
	must(t, err)
	return s
}

// The list holds the current serial from boot, so workers enforce it.
func TestControllerSerialsSetAtBoot(t *testing.T) {
	o := newClusterPanel(t)
	if got := o.cluster.serials(); !slices.Equal(got, []string{ctlSerial(t, o)}) {
		t.Fatalf("announced %v", got)
	}
	_, s := o.meshTrust(context.Background())
	if len(s) != 1 {
		t.Fatalf("spec serials %v", s)
	}
}

// Nothing due: the daily call announces the current serial and pushes it once.
func TestAnnounceSteady(t *testing.T) {
	o, an := ctlRig(t)
	cur := ctlSerial(t, o)
	must(t, o.announceControllerCert(context.Background()))
	must(t, o.announceControllerCert(context.Background()))
	if ctlSerial(t, o) != cur || len(an.seen) != 1 || !slices.Equal(an.seen[0], []string{cur}) {
		t.Fatalf("seen %v", an.seen)
	}
}

// A due cert: both serials go out while the old cert is in use, the switch
// follows, then the new serial alone.
func TestRenewAnnouncesBothSwitchesThenNext(t *testing.T) {
	o, an := ctlRig(t)
	old := ctlSerial(t, o)
	// Make the cert due: the CA no longer vouches for it.
	other, err := clusterca.NewCA(time.Now())
	must(t, err)
	o.cluster.ca = other
	must(t, o.announceControllerCert(context.Background()))
	next := ctlSerial(t, o)
	if next == old {
		t.Fatal("not switched")
	}
	if len(an.seen) != 2 {
		t.Fatalf("pushes %v", an.seen)
	}
	if !slices.Equal(an.seen[0], []string{old, next}) || an.cert[0] != old {
		t.Errorf("first push %v while using %s", an.seen[0], an.cert[0])
	}
	if !slices.Equal(an.seen[1], []string{next}) || an.cert[1] != next {
		t.Errorf("second push %v while using %s", an.seen[1], an.cert[1])
	}
	if got := o.cluster.serials(); !slices.Equal(got, []string{next}) {
		t.Errorf("announced %v", got)
	}
	es, err := o.SecurityLog(context.Background(), SecurityFilter{Action: securitylog.ControllerCertRenew})
	must(t, err)
	if len(es) != 1 {
		t.Errorf("renew events %d", len(es))
	}
}

// Revoke is an immediate renewal; the old serial leaves the list and is
// revoked.
func TestRevokeDropsOldSerial(t *testing.T) {
	o, an := ctlRig(t)
	old := ctlSerial(t, o)
	must(t, o.RevokeControllerCert(context.Background(), nil))
	o.trustAsync.Wait()
	next := ctlSerial(t, o)
	if next == old {
		t.Fatal("not replaced")
	}
	if last := an.seen[len(an.seen)-1]; !slices.Equal(last, []string{next}) {
		t.Errorf("last announce %v", last)
	}
	if rev, _ := o.store.NodeCerts.IsRevoked(context.Background(), old); !rev {
		t.Error("old serial not revoked")
	}
	es, err := o.SecurityLog(context.Background(), SecurityFilter{Action: securitylog.ControllerCertRevoke})
	must(t, err)
	if len(es) != 1 {
		t.Errorf("revoke events %d", len(es))
	}
}

// A worker that never answers delays the switch by the bound, no longer.
func TestRenewNotBlockedByDeadWorker(t *testing.T) {
	o, an := ctlRig(t)
	an.fail = true
	old := ctlSerial(t, o)
	start := time.Now()
	must(t, o.RevokeControllerCert(context.Background(), nil))
	o.trustAsync.Wait()
	if ctlSerial(t, o) == old {
		t.Fatal("not switched")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %v", d)
	}
}

// With a worker that never answers, revoke and rotate still return at once
// (the switch waits detached, past the request); a second call while one
// waits is refused; Close cancels the wait.
func TestTrustVerbsReturnWhileWorkerDead(t *testing.T) {
	defer func(b time.Duration) { controllerAckBound = b }(controllerAckBound)
	o, an := ctlRig(t)
	an.fail = true
	controllerAckBound = time.Hour
	start := time.Now()
	must(t, o.RevokeControllerCert(context.Background(), nil))
	if err := o.RevokeControllerCert(context.Background(), nil); err == nil {
		t.Fatal("a second revoke ran beside the first")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("revoke blocked %v", d)
	}
	if err := o.BreakGlass(context.Background(), adminP()); err == nil {
		t.Fatal("break glass ran beside a revoke")
	}
	must(t, o.Close())
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("close waited %v", d)
	}

	o, an = ctlRig(t)
	an.fail = true
	controllerAckBound = time.Hour
	start = time.Now()
	must(t, o.RotateRoot(context.Background(), adminP()))
	if err := o.RotateRoot(context.Background(), adminP()); err == nil {
		t.Fatal("a second rotate ran beside the first")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("rotate blocked %v", d)
	}
	if err := o.FinishRollover(context.Background(), adminP()); err == nil {
		t.Fatal("finish ran beside the rollover's pushes")
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("finish blocked %v", d)
	}
	must(t, o.Close())
}
