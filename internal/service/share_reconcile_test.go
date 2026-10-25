package service

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
)

// reconcileJob is whether a share reconcile job has ended in state.
func reconcileJob(n *twoNodes, state string) bool {
	js, _ := n.orch.Jobs(context.Background(), 50, state)
	return slices.ContainsFunc(js, func(j Job) bool { return j.Kind == string(kindShareReconcile) })
}

// shareOnWorker is a share with a volume of its recipe on the worker, beside
// a plain env volume that must never be touched.
func shareOnWorker(t *testing.T, n *twoNodes) (org, vol string) {
	t.Helper()
	ctx := context.Background()
	st, err := n.st.Stacks.Get(ctx, n.stack)
	must(t, err)
	s, err := n.orch.CreateShare(ctx, st.OrgID, ShareSpec{Slug: "media", Kind: "nfs", Source: "nas:/e"})
	must(t, err)
	vol = volume.ShareVolumeName(s, "a")
	n.wFake.Volumes = []docker.VolumeInfo{
		{Name: vol, Labels: map[string]string{volume.ShareLabel: s.ID}},
		{Name: "stackr-vol-x", Labels: map[string]string{"stackr.volume": "x"}},
	}
	return st.OrgID, vol
}

// Deleting a share removes its volume from a worker, not only the controller's.
func TestDeleteShareRemovesWorkerVolume(t *testing.T) {
	n := newTwoNodes(t)
	org, vol := shareOnWorker(t, n)
	must(t, n.orch.DeleteShare(context.Background(), org, "media"))
	waitFor(t, "worker volume gone", func() bool { return sawCall(n.wFake, "RemoveVolume("+vol+")") })
	if sawCall(n.wFake, "RemoveVolume(stackr-vol-x") {
		t.Error("a plain volume was removed")
	}
}

// A worker that is offline at delete time is cleaned when it reconnects.
func TestOfflineWorkerShareVolumeCleanedOnReconnect(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	org, vol := shareOnWorker(t, n)
	w, err := n.st.Nodes.Get(ctx, "w1")
	must(t, err)
	w.Status = node.StatusOffline
	must(t, n.st.Nodes.Update(ctx, w))
	n.wFake.Err = map[string]error{"ListVolumes": errors.New("node unreachable")}

	must(t, n.orch.DeleteShare(ctx, org, "media"))
	waitFor(t, "the reconcile ran", func() bool { return reconcileJob(n, "done") })
	if sawCall(n.wFake, "RemoveVolume(") {
		t.Fatal("an offline node was reached")
	}

	delete(n.wFake.Err, "ListVolumes")
	w.Status = node.StatusOnline
	must(t, n.st.Nodes.Update(ctx, w))
	n.orch.reconcileShares(ctx) // what refreshNodeClients does when the node comes back
	waitFor(t, "worker volume gone", func() bool { return sawCall(n.wFake, "RemoveVolume("+vol+")") })
}

// A volume a container holds is skipped and the job fails so it runs again.
func TestShareReconcileSkipsInUse(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	org, _ := shareOnWorker(t, n)
	n.wFake.Err = map[string]error{"RemoveVolume": errors.New("volume is in use")}
	must(t, n.orch.DeleteShare(ctx, org, "media"))
	waitFor(t, "the reconcile failed", func() bool { return reconcileJob(n, "failed") })
	delete(n.wFake.Err, "RemoveVolume")
	n.orch.reconcileShares(ctx)
	waitFor(t, "the retry done", func() bool { return reconcileJob(n, "done") })
}
