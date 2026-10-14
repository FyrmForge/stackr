package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/clusterca"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// rolloverRig is a panel with workers w1 and w2, each holding a cert from the
// original root.
func rolloverRig(t *testing.T) *Orchestrator {
	t.Helper()
	old := rolloverSettle
	rolloverSettle = 0
	t.Cleanup(func() { rolloverSettle = old })
	o := newClusterPanel(t)
	for i, id := range []string{"w1", "w2"} {
		must(t, o.store.Nodes.Create(context.Background(), store.Node{
			ID: id, Name: id, Slug: id, Role: node.Worker, Tags: "[]", RangeCIDR: fmt.Sprintf("10.%d.0.0/16", 209+i), WGAddress: fmt.Sprintf("10.207.255.%d", 2+i),
			WGPublicKey: "wg-" + id, Status: node.StatusOnline, CreatedAt: time.Now(),
		}))
		workerAsks(t, o, id)
	}
	return o
}

// workerAsks is the node asking for a renewal: the new cert is signed by the
// controller's current root.
func workerAsks(t *testing.T, o *Orchestrator, id string) {
	t.Helper()
	_, err := o.renewNodeCert(context.Background(), id, []byte(newCSR(t)), "test")
	must(t, err)
}

func rootFP(o *Orchestrator) string { return clusterca.Fingerprint(o.cluster.ca.Cert) }

func auditActions(t *testing.T, o *Orchestrator, action string) []SecurityEvent {
	t.Helper()
	es, err := o.SecurityLog(context.Background(), SecurityFilter{Action: action})
	must(t, err)
	return es
}

func TestRotateRootAllOnline(t *testing.T) {
	ctx := context.Background()
	o := rolloverRig(t)
	oldFP := rootFP(o)
	must(t, o.RotateRoot(ctx, adminP()))
	o.trustAsync.Wait()
	newFP := rootFP(o)
	if newFP == oldFP {
		t.Fatal("the root did not change")
	}
	if len(o.cluster.roots()) != 2 {
		t.Fatalf("roots during rollover = %d, want old and new", len(o.cluster.roots()))
	}
	if st, _, _ := o.store.ClusterCA.Get(ctx); !st.RollingOver() || st.PreviousFP != oldFP || st.CurrentFP != newFP {
		t.Fatalf("state = %+v", st)
	}
	if err := o.RotateRoot(ctx, adminP()); err == nil {
		t.Fatal("a second rollover started while one runs")
	}
	// The controller cert is already from the new root; the workers still ask.
	own, err := clusterca.ParseCertPEM(mustRead(o.cfg.DataDir + "/" + controllerCertFile))
	if err != nil || own.CheckSignatureFrom(o.cluster.ca.Cert) != nil {
		t.Fatalf("controller cert not from the new root: %v", err)
	}
	info, err := o.ClusterTrust(ctx, adminP())
	must(t, err)
	if !info.RollingOver || len(info.OnOldRoot) != 2 {
		t.Fatalf("info = %+v", info)
	}
	workerAsks(t, o, "w1")
	workerAsks(t, o, "w2")
	must(t, o.rootRolloverTick(ctx))
	if got := o.cluster.roots(); len(got) != 1 || clusterca.Fingerprint(got[0]) != newFP {
		t.Fatalf("roots after finish = %d", len(got))
	}
	if _, err := clusterca.LoadPrevCA(o.cfg.DataDir); !os.IsNotExist(err) {
		t.Fatalf("previous root still on disk: %v", err)
	}
	st, _, _ := o.store.ClusterCA.Get(ctx)
	if st.RollingOver() || st.PreviousFP != "" || st.RolloverFinishedAt == nil {
		t.Fatalf("state after finish = %+v", st)
	}
	cs, err := o.store.NodeCerts.ListLive(ctx)
	must(t, err)
	for _, id := range []string{"w1", "w2"} {
		var last store.NodeCert
		for _, c := range cs {
			if c.NodeID == id {
				last = c
			}
		}
		if last.Issuer != newFP {
			t.Fatalf("%s newest cert issuer = %q", id, last.Issuer)
		}
	}
	for _, a := range []string{securitylog.RootRolloverStart, securitylog.RootRolloverPush, securitylog.RootRolloverFinish} {
		if n := len(auditActions(t, o, a)); n != 1 {
			t.Errorf("%s events = %d, want 1", a, n)
		}
	}
}

// One worker is offline and has not asked: the old root stays until it does.
func TestRotateRootWaitsForOfflineNode(t *testing.T) {
	ctx := context.Background()
	o := rolloverRig(t)
	must(t, o.RotateRoot(ctx, adminP()))
	o.trustAsync.Wait()
	workerAsks(t, o, "w1")
	must(t, o.rootRolloverTick(ctx))
	if len(o.cluster.roots()) != 2 {
		t.Fatal("the old root was dropped while w2 still holds a cert from it")
	}
	if _, err := clusterca.LoadPrevCA(o.cfg.DataDir); err != nil {
		t.Fatalf("previous root gone: %v", err)
	}
	info, err := o.ClusterTrust(ctx, adminP())
	must(t, err)
	if len(info.OnOldRoot) != 1 || info.OnOldRoot[0].Name != "w2" {
		t.Fatalf("old root nodes = %v", info.OnOldRoot)
	}
	workerAsks(t, o, "w2")
	must(t, o.rootRolloverTick(ctx))
	if len(o.cluster.roots()) != 1 {
		t.Fatal("the old root stayed after w2 moved")
	}
}

// A new-root cert younger than the settle time does not count yet.
func TestRotateRootSettle(t *testing.T) {
	ctx := context.Background()
	o := rolloverRig(t)
	must(t, o.RotateRoot(ctx, adminP()))
	o.trustAsync.Wait()
	workerAsks(t, o, "w1")
	workerAsks(t, o, "w2")
	rolloverSettle = time.Hour
	must(t, o.rootRolloverTick(ctx))
	if len(o.cluster.roots()) != 2 {
		t.Fatal("dropped the old root before the new certs settled")
	}
}

func TestForceFinishMarksNeedsRejoin(t *testing.T) {
	ctx := context.Background()
	o := rolloverRig(t)
	if err := o.FinishRollover(ctx, adminP()); err == nil {
		t.Fatal("force finish without a rollover")
	}
	must(t, o.RotateRoot(ctx, adminP()))
	o.trustAsync.Wait()
	workerAsks(t, o, "w1")
	must(t, o.FinishRollover(ctx, adminP()))
	if len(o.cluster.roots()) != 1 {
		t.Fatal("old root kept after a forced finish")
	}
	w2, err := o.store.Nodes.Get(ctx, "w2")
	must(t, err)
	w1, err := o.store.Nodes.Get(ctx, "w1")
	must(t, err)
	if w2.Status != StateNeedsRejoin || w1.Status == StateNeedsRejoin {
		t.Fatalf("statuses w1=%q w2=%q", w1.Status, w2.Status)
	}
	if es := auditActions(t, o, securitylog.RootRolloverNode); len(es) != 1 || es[0].TargetID != "w2" {
		t.Fatalf("node events = %+v", es)
	}
	if es := auditActions(t, o, securitylog.RootRolloverFinish); len(es) != 1 || !strings.Contains(es[0].Detail, "by an admin") {
		t.Fatalf("finish events = %+v", es)
	}
}

func TestRolloverTickStartsUnderAYear(t *testing.T) {
	ctx := context.Background()
	o := rolloverRig(t)
	must(t, o.rootRolloverTick(ctx))
	if _, ok, _ := o.store.ClusterCA.Get(ctx); ok {
		t.Fatal("a no-op tick wrote cluster_ca")
	}
	// A root with 11 months left.
	short, err := clusterca.NewCA(time.Now().Add(-clusterca.CALifetime + 11*30*24*time.Hour))
	must(t, err)
	o.cluster.swapCA(short, nil)
	must(t, clusterca.SaveCA(o.cfg.DataDir, short))
	must(t, o.rootRolloverTick(ctx))
	if es := auditActions(t, o, securitylog.RootRolloverStart); len(es) != 1 || es[0].ActorID != "" {
		t.Fatalf("start events = %+v", es)
	}
	if rootFP(o) == clusterca.Fingerprint(short.Cert) {
		t.Fatal("still on the short root")
	}
}

// A panel restart mid-rollover: the cluster rebuilt from disk boots with both
// roots, and the tick finishes it.
func TestRolloverResumesAfterRestart(t *testing.T) {
	ctx := context.Background()
	o := rolloverRig(t)
	must(t, o.RotateRoot(ctx, adminP()))
	o.trustAsync.Wait()
	workerAsks(t, o, "w1")
	workerAsks(t, o, "w2")
	pki, err := bootCluster(ctx, o.store, o.cfg.DataDir)
	must(t, err)
	o.cluster = pki
	if len(o.cluster.roots()) != 2 {
		t.Fatalf("boot trust list = %d, want both roots", len(o.cluster.roots()))
	}
	must(t, o.rootRolloverTick(ctx))
	if len(o.cluster.roots()) != 1 {
		t.Fatal("rollover not finished after the restart")
	}
}

// A crash between RotateCA and the cluster_ca write is mended from the key
// files.
func TestRolloverReconcilesFromDisk(t *testing.T) {
	ctx := context.Background()
	o := rolloverRig(t)
	old := o.cluster.ca
	next, err := clusterca.NewCA(time.Now())
	must(t, err)
	must(t, clusterca.RotateCA(o.cfg.DataDir, next))
	o.cluster.swapCA(next, nil)
	st, err := o.reconcileRollover(ctx)
	must(t, err)
	if !st.RollingOver() || st.PreviousFP != clusterca.Fingerprint(old.Cert) || len(o.cluster.roots()) != 2 {
		t.Fatalf("state = %+v roots = %d", st, len(o.cluster.roots()))
	}
}

func TestJoinCommandCarriesNewFingerprint(t *testing.T) {
	ctx := context.Background()
	o := rolloverRig(t)
	o.cfg.BaseURL = "https://panel.test"
	must(t, o.RotateRoot(ctx, adminP()))
	o.trustAsync.Wait()
	out, err := o.CreateJoinToken(ctx, adminP(), JoinTokenSpec{NodeName: "w3"})
	must(t, err)
	if out.Fingerprint != rootFP(o) || !strings.Contains(out.Command, "--ca-sha256 "+rootFP(o)) {
		t.Fatalf("join = %+v", out)
	}
}

// The cross cert is written before the old key leaves: when it cannot be
// written, nothing has rotated and the old root still signs.
func TestRolloverCrossFailureRotatesNothing(t *testing.T) {
	ctx := context.Background()
	o := rolloverRig(t)
	old := clusterca.Fingerprint(o.cluster.ca.Cert)
	must(t, os.MkdirAll(filepath.Join(o.cfg.DataDir, crossFile, "x"), 0o755)) // a dir: the write fails
	if err := o.RotateRoot(ctx, adminP()); err == nil {
		t.Fatal("rotated with no cross cert")
	}
	ca, err := clusterca.LoadCA(o.cfg.DataDir)
	must(t, err)
	if clusterca.Fingerprint(ca.Cert) != old {
		t.Fatal("the CA on disk moved although the rollover failed")
	}
}
