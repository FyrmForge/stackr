package service

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

// A recovery that carries a new key still answers while the push to the other
// nodes is stuck (another sync holds the push lock, or peers are mid-rotation).
func TestRecoverAnswersWhilePeerSyncBlocks(t *testing.T) {
	ctx := context.Background()
	o := newClusterPanel(t)
	o.recoverSyncFor = 50 * time.Millisecond
	must(t, o.store.Nodes.Create(ctx, store.Node{
		ID: "w1", Name: "w1", Slug: "w1", Role: node.Worker, Tags: "[]", RangeCIDR: "10.209.0.0/16", WGAddress: "10.207.255.2",
		WGPublicKey: pubKey(t), Status: node.StatusOnline, CreatedAt: time.Now(),
	}))
	o.stopMesh()
	o.mesh.syncMu.Lock()
	defer o.mesh.syncMu.Unlock()
	must(t, o.markRotation(ctx, "w1", "schedule"))
	req, err := worker.SignRecover(nodeCert(t, o, "w1"), worker.RecoverPayload{Time: time.Now().Unix(), Nonce: "b1", Purpose: worker.PurposeRecover, WGPublic: pubKey(t), ExitPublic: pubKey(t)})
	must(t, err)
	start := time.Now()
	if _, err := o.RecoverNode(ctx, req); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("the answer waited %s on the peer push", d)
	}
}

// A worker that reverted to the key the panel held before a rotation is taken
// back with it, once; any other key, the same swap again, or the old key after
// the window is refused.
func TestRecoverAcceptsRevertedWorker(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	o := newClusterPanel(t)
	old, fresh := pubKey(t), pubKey(t)
	must(t, o.store.Nodes.Create(ctx, store.Node{
		ID: "w1", Name: "w1", Slug: "w1", Role: node.Worker, Tags: "[]", RangeCIDR: "10.209.0.0/16", WGAddress: "10.207.255.2",
		WGPublicKey: old, Status: node.StatusOnline, CreatedAt: time.Now(),
	}))
	own := nodeCert(t, o, "w1")
	nonce := 0
	call := func(wg string) error {
		nonce++
		req, err := worker.SignRecover(own, worker.RecoverPayload{Time: time.Now().Unix(), Nonce: "v" + strconv.Itoa(nonce), Purpose: worker.PurposeRecover, WGPublic: wg, ExitPublic: pubKey(t)})
		must(t, err)
		_, err = o.RecoverNode(ctx, req)
		return err
	}
	held := func() string { n, _ := o.store.Nodes.Get(ctx, "w1"); return n.WGPublicKey }
	must(t, o.markRotation(ctx, "w1", "schedule"))
	must(t, call(fresh))
	if held() != fresh {
		t.Fatal("new key not taken")
	}
	if err := call(pubKey(t)); !errors.Is(err, errs.ErrRefused) || held() != fresh {
		t.Fatalf("unrelated key: err = %v", err)
	}
	must(t, call(old)) // the worker reverted
	if held() != old {
		t.Fatal("reverted key not taken back")
	}
	if err := call(fresh); !errors.Is(err, errs.ErrRefused) || held() != old {
		t.Fatalf("swap without a rotation: err = %v", err)
	}
	must(t, o.markRotation(ctx, "w1", "schedule"))
	must(t, call(fresh))
	must(t, o.store.Settings.Set(ctx, prevKeyKey+"w1", "1|"+old))
	if err := call(old); !errors.Is(err, errs.ErrRefused) || held() != fresh {
		t.Fatalf("expired window: err = %v", err)
	}
}
