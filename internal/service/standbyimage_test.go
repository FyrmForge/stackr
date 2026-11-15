package service

import (
	"context"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
)

// imageRig is an instance with a standby on w1 whose container carries the
// settings label the spec has now and runs from the image the tile had; the
// tile then moves to the next minor, as an update does.
type imageRig struct {
	n        *twoNodes
	t        Tile
	m        InstanceMember
	old, new string
}

func newImageRig(t *testing.T, label, primaryNew bool) *imageRig {
	t.Helper()
	ctx := context.Background()
	n := newTwoNodes(t)
	in := pgOn(t, n)
	_, err := n.orch.operated.SetMajor(ctx, in, mflow.ImageMajor(mflow.Engines["postgres"].Definition().Image))
	must(t, err)
	r, err := n.orch.AddStandby(ctx, adminP(), n.devT.ID, "w1")
	must(t, err)
	n.orch.vipAsync.Wait()
	old := mflow.Engines["postgres"].Definition().Image
	nw, ok := mflow.PGUpdate(old)
	if !ok {
		t.Skip("no newer minor known")
	}
	must(t, n.st.Tiles.SetImageRef(ctx, n.devT.ID, nw))
	tl, _ := n.orch.tiles.Get(ctx, n.devT.ID)
	host, err := n.orch.memberHost(ctx, r, r.NodeID)
	must(t, err)
	spec, err := n.orch.engines.StandbySpec(ctx, tl, r, host)
	must(t, err)
	lbl := map[string]string{LabelMember: r.ID, mflow.LabelConf: spec.Labels[mflow.LabelConf]}
	if label {
		lbl[mflow.LabelImage] = old
	}
	n.wFake.Containers = append(n.wFake.Containers, docker.Container{ID: "rep1", Name: "rep1", State: "running", Image: old, Labels: lbl})
	pimg := old
	if primaryNew {
		pimg = nw
	}
	for i := range n.ctlFake.Containers {
		if n.ctlFake.Containers[i].ID == "pg" {
			n.ctlFake.Containers[i].Image = pimg
		}
	}
	n.ctlFake.ExecFn = func(cmd []string) string {
		if strings.Contains(strings.Join(cmd, " "), "current_setting") {
			return "max_connections|0|100|postmaster\nmax_worker_processes|0|8|postmaster\nmax_wal_senders|0|10|postmaster\nmax_prepared_transactions|0|0|postmaster\nmax_locks_per_transaction|0|64|postmaster\n"
		}
		return "0"
	}
	return &imageRig{n, tl, r, old, nw}
}

// recreated says whether a standby container was made on the new image.
func (g *imageRig) recreated() bool {
	for _, c := range g.n.wFake.Calls() {
		if c.Method == "Create" && strings.Contains(strings.Join(c.Args, " "), g.new) {
			return true
		}
	}
	return false
}

// A running standby is made again on the instance's new image once the
// primary runs on it, with no restart pending.
func TestStandbyFollowsImage(t *testing.T) {
	t.Parallel()
	g := newImageRig(t, true, true)
	g.n.orch.syncStandbyConf(context.Background(), g.t)
	if !g.recreated() {
		t.Errorf("standby stayed on %s: %v", g.old, callNames(g.n.wFake))
	}
}

// A standby does not move to an image its primary is not on (the update
// failed there), and one with no image label is left alone.
func TestStandbyImageHeld(t *testing.T) {
	t.Parallel()
	for name, g := range map[string]*imageRig{
		"primary still on the old image": newImageRig(t, true, false),
		"container without the label":    newImageRig(t, false, true),
	} {
		g.n.orch.syncStandbyConf(context.Background(), g.t)
		if g.recreated() {
			t.Errorf("%s: the standby was made again", name)
		}
	}
}

// A stopped standby on an old image is made again at its start.
func TestStoppedStandbyFollowsImage(t *testing.T) {
	t.Parallel()
	g := newImageRig(t, true, true)
	for i := range g.n.wFake.Containers {
		if g.n.wFake.Containers[i].ID == "rep1" {
			g.n.wFake.Containers[i].State = "exited"
		}
	}
	must(t, g.n.orch.startMember(context.Background(), g.m))
	if !g.recreated() {
		t.Errorf("stopped standby kept its image: %v", callNames(g.n.wFake))
	}
}
