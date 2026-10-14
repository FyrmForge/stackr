package service

import (
	"context"
	"crypto/x509"
	"slices"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/clusterca"
	"github.com/FyrmForge/stackr/internal/worker"
)

// specNode keeps every spec it is pushed and the controller cert in use then.
type specNode struct {
	fakeMeshNode
	o      *Orchestrator
	serial []string
}

func (s *specNode) ApplyMesh(ctx context.Context, sp worker.MeshSpec) error {
	cur, _ := certSerial(s.o.cluster.cert())
	s.serial = append(s.serial, cur)
	return s.fakeMeshNode.ApplyMesh(ctx, sp)
}

// H3: a reachable worker gets one last spec, over the old cert, that names
// only the new root and the new controller serial.
func TestBreakGlassPushesNewTrustToReachableWorkers(t *testing.T) {
	ctx := context.Background()
	o, _ := ctlRig(t)
	sn := &specNode{o: o}
	o.mesh.nodes = func(id string) (MeshNode, bool) { return sn, id == "w1" }
	oldSerial := ctlSerial(t, o)
	must(t, o.BreakGlass(ctx, adminP()))

	if len(sn.pushed) == 0 {
		t.Fatal("the worker was not told about the new root")
	}
	sp, used := sn.pushed[0], sn.serial[0]
	if used != oldSerial {
		t.Errorf("the last push went over serial %s, want the old %s", used, oldSerial)
	}
	cur, err := clusterca.LoadCA(o.cfg.DataDir)
	must(t, err)
	want := clusterca.RootsToPEM([]*x509.Certificate{cur.Cert})
	if !slices.Equal(sp.TrustRoots, want) {
		t.Errorf("pushed %d roots, want only the new one", len(sp.TrustRoots))
	}
	if newSerial := ctlSerial(t, o); newSerial == oldSerial || !slices.Equal(sp.ControllerSerials, []string{newSerial}) {
		t.Errorf("pushed serials %v, want only the new controller cert", sp.ControllerSerials)
	}
}

// M7: break glass refuses while a rollover is detached and waits for rolloverMu.
func TestBreakGlassRefusesDuringRollover(t *testing.T) {
	ctx := context.Background()
	o := newClusterPanel(t)
	rolloverDetached.Store(true)
	err := o.BreakGlass(ctx, adminP())
	rolloverDetached.Store(false)
	if err == nil {
		t.Fatal("break glass ran while a rollover was detached")
	}
	rolloverMu.Lock()
	done := make(chan error, 1)
	go func() { done <- o.BreakGlass(ctx, adminP()) }()
	select {
	case <-done:
		rolloverMu.Unlock()
		t.Fatal("break glass ran while rolloverMu was held")
	case <-time.After(150 * time.Millisecond):
	}
	rolloverMu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// M8: a join request spent before the incident is rejected, not left pending.
func TestBreakGlassRejectsPendingJoins(t *testing.T) {
	ctx := context.Background()
	o := newClusterPanel(t)
	adm := adminP()
	out, err := o.CreateJoinToken(ctx, adm, JoinTokenSpec{NodeName: "w2"})
	must(t, err)
	_, err = o.RequestJoin(ctx, httpsMeta, joinReq(t, out.Token))
	must(t, err)
	must(t, o.BreakGlass(ctx, adm))
	if p, _ := o.PendingJoins(ctx, adm); len(p) != 0 {
		t.Errorf("%d joins still pending", len(p))
	}
	if s, err := o.JoinStatus(ctx, out.Token); err != nil || s.Status != JoinRejected {
		t.Errorf("status = %+v %v, want rejected", s, err)
	}
}
