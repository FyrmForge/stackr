package service

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/wgapply"
	"github.com/FyrmForge/stackr/internal/worker"
)

func pubKey(t *testing.T) string {
	t.Helper()
	k, err := wgtypes.GeneratePrivateKey()
	must(t, err)
	return k.PublicKey().String()
}

// A worker that rotated reports its new public keys through the panel: the
// controller stores them, drops the old key's traffic samples, audits the
// rotation with the trigger it was asked with, and refuses a key that is
// not one.
func TestRecoverTakesRotatedKeys(t *testing.T) {
	ctx := context.Background()
	o := newClusterPanel(t)
	old, peer := pubKey(t), "peer-of-w1"
	must(t, o.store.Nodes.Create(ctx, store.Node{
		ID: "w1", Name: "w1", Slug: "w1", Role: node.Worker, Tags: "[]", RangeCIDR: "10.209.0.0/16", WGAddress: "10.207.255.2",
		WGPublicKey: old, Status: node.StatusOnline, CreatedAt: time.Now(),
	}))
	o.mesh.mu.Lock()
	o.mesh.rx["w2"] = map[string]rxSample{old: {n: 5}, peer: {n: 7}}
	o.mesh.mu.Unlock()
	own := nodeCert(t, o, "w1")
	o.rotation.trigger.Store("w1", "manual by admin@test.com")
	wg, exit := pubKey(t), pubKey(t)
	req, err := worker.SignRecover(own, worker.RecoverPayload{Time: time.Now().Unix(), Nonce: "r1", Purpose: worker.PurposeRecover, WGPublic: wg, ExitPublic: exit})
	must(t, err)
	if _, err := o.RecoverNode(ctx, req); err != nil {
		t.Fatal(err)
	}
	n, err := o.store.Nodes.Get(ctx, "w1")
	must(t, err)
	if n.WGPublicKey != wg {
		t.Fatal("new key not stored")
	}
	o.mesh.mu.Lock()
	_, stale := o.mesh.rx["w2"][old]
	_, kept := o.mesh.rx["w2"][peer]
	gotExit := o.mesh.exitPK["w1"]
	o.mesh.mu.Unlock()
	if stale || !kept || gotExit != exit {
		t.Fatalf("mesh state: stale=%v kept=%v exit ok=%v", stale, kept, gotExit == exit)
	}
	if _, ok := o.rotation.trigger.Load("w1"); ok {
		t.Fatal("trigger not consumed")
	}
	evs, err := o.SecurityLog(ctx, SecurityFilter{})
	must(t, err)
	var row string
	for _, e := range evs {
		if e.Action == "node.key_rotate" {
			row = e.Detail
		}
	}
	if !strings.Contains(row, old[:8]+" -> "+wg[:8]) || !strings.Contains(row, "manual by admin@test.com") {
		t.Fatalf("audit = %q", row)
	}

	req, err = worker.SignRecover(own, worker.RecoverPayload{Time: time.Now().Unix(), Nonce: "r2", Purpose: worker.PurposeRecover, WGPublic: "not-a-key", ExitPublic: exit})
	must(t, err)
	if _, err := o.RecoverNode(ctx, req); !errors.Is(err, errs.ErrRefused) {
		t.Fatalf("bad key: err = %v", err)
	}
}

// orderNode records the order of pushes and the controller key each saw.
type orderNode struct {
	id    string
	log   *[]string
	ctlID string
	lost  bool
}

func (n orderNode) Facts(context.Context) (worker.Facts, error)       { return worker.Facts{}, nil }
func (n orderNode) MeshInfo(context.Context) (worker.MeshInfo, error) { return worker.MeshInfo{}, nil }
func (n orderNode) ApplyMesh(ctx context.Context, sp worker.MeshSpec) error {
	for _, p := range sp.Peers {
		if p.NodeID == n.ctlID {
			*n.log = append(*n.log, n.id+" "+p.PublicKey)
		}
	}
	if n.lost { // applied, but the reply crossed the rekeyed tunnel
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

// The controller stages its keys, pushes the workers the new public key
// farthest first, then switches itself.
func TestRotateControllerOrder(t *testing.T) {
	ctx := context.Background()
	o := newClusterPanel(t)
	ctl, err := o.controllerID(ctx)
	must(t, err)
	c, err := o.store.Nodes.Get(ctx, ctl)
	must(t, err)
	c.RangeCIDR = "10.208.0.0/16"
	must(t, o.store.Nodes.Update(ctx, c))
	for i, id := range []string{"w1", "w2"} {
		must(t, o.store.Nodes.Create(ctx, store.Node{ID: id, Name: id, Slug: id, Role: node.Worker, Tags: "[]",
			RangeCIDR: []string{"10.209.0.0/16", "10.210.0.0/16"}[i], WGAddress: []string{"10.207.255.2", "10.207.255.3"}[i], WGPublicKey: pubKey(t), Status: node.StatusOnline, CreatedAt: time.Now()}))
	}
	rec := &wgapply.Recorder{}
	o.localWorker = worker.New(worker.Config{StateDir: t.TempDir(), Mesh: worker.MeshConfig{WG: rec}})
	t.Cleanup(o.localWorker.Stop)
	info, err := o.localWorker.MeshInfo()
	must(t, err)
	// The panel's own mesh loop would re-plan over the state set below:
	// stop it and wait out a sync in flight (Run returns after it).
	o.stopMesh()
	o.mesh.syncMu.Lock()
	o.mesh.syncMu.Unlock() //nolint:staticcheck // a barrier, not a section
	var log []string
	o.mesh.nodes = func(id string) (MeshNode, bool) {
		if id == ctl {
			return localMesh{o.localWorker}, true
		}
		return orderNode{id: id, log: &log, ctlID: ctl, lost: true}, true
	}
	// Every worker's reply is lost, as for a direct worker on the rig: the
	// rotation must not wait the client's timeout for each.
	defer func(w time.Duration) { rotatePushWait = w }(rotatePushWait)
	rotatePushWait = 50 * time.Millisecond
	o.mesh.mu.Lock()
	o.mesh.wgPK[ctl], o.mesh.exitPK[ctl] = info.WGPublic, info.ExitPublic
	o.mesh.next = map[[2]string]string{{ctl, "w2"}: "w1", {"w2", ctl}: "w1"} // w2 is routed via w1
	o.mesh.mu.Unlock()

	start := time.Now()
	must(t, o.rotateController(ctx, c, "schedule"))
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("the rotation waited %s on lost replies", d)
	}
	after, err := o.localWorker.MeshInfo()
	must(t, err)
	if after.WGPublic == info.WGPublic {
		t.Fatal("the controller kept its key")
	}
	if len(log) != 2 || !strings.HasPrefix(log[0], "w2 ") || !strings.HasPrefix(log[1], "w1 ") ||
		!strings.HasSuffix(log[0], after.WGPublic) || !strings.HasSuffix(log[1], after.WGPublic) {
		t.Fatalf("pushes: want w2 then w1, both with the new key; got %d pushes", len(log))
	}
}

// The controller pushes its new key to the farthest workers first.
func TestHops(t *testing.T) {
	next := map[[2]string]string{{"c", "w3"}: "w2", {"w2", "w3"}: "w1"}
	if hops(next, "c", "w1") != 1 || hops(next, "c", "w3") != 3 {
		t.Fatalf("hops = %d, %d", hops(next, "c", "w1"), hops(next, "c", "w3"))
	}
}

// The schedule: off at 0, starts counting from its first run, due after
// its days.
func TestRotateDue(t *testing.T) {
	ctx := context.Background()
	o := newClusterPanel(t)
	now := time.Now()
	if o.rotateDue(ctx, now) {
		t.Fatal("due with no schedule")
	}
	must(t, o.store.Settings.Set(ctx, RotateEveryKey, "7"))
	if o.rotateDue(ctx, now) {
		t.Fatal("due on the schedule's first look")
	}
	if o.rotateDue(ctx, now.Add(6*24*time.Hour)) || !o.rotateDue(ctx, now.Add(7*24*time.Hour+time.Minute)) {
		t.Fatal("wrong due time")
	}
}

// The gate between nodes: a pair that just went down holds it, an old one
// does not; the last rotated key must carry traffic or a minute pass.
func TestRotationSettled(t *testing.T) {
	m := newMeshSvc(nil, nil)
	now := time.Now()
	m.down[[2]string{"a", "b"}] = now.Add(-time.Hour)
	if !m.settled(now) {
		t.Fatal("an old dead pair held the gate")
	}
	m.down[[2]string{"a", "c"}] = now.Add(-10 * time.Second)
	if m.settled(now) {
		t.Fatal("a fresh dead pair did not hold the gate")
	}
	delete(m.down, [2]string{"a", "c"})
	m.tookKeys("x", "old", "new", "exit")
	if m.settled(time.Now()) {
		t.Fatal("passed before the new key was heard")
	}
	m.rx["y"] = map[string]rxSample{"new": {n: 1, changed: time.Now()}}
	if !m.settled(time.Now()) {
		t.Fatal("held after the new key carried traffic")
	}
}
