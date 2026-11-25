package service

import (
	"context"
	"errors"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/opmember"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
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

// prodID is what a primary's container reports as its image on a real
// install: an image ID (or the registry copy), never the tag.
const prodID = "sha256:0123456789abcdef"

func newImageRig(t *testing.T, label, primaryNew bool) *imageRig {
	t.Helper()
	return newImageRigOn(t, newTwoNodes(t), label, primaryNew)
}

func newImageRigOn(t *testing.T, n *twoNodes, label, primaryNew bool) *imageRig {
	t.Helper()
	ctx := context.Background()
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
	g := &imageRig{n: n, t: tl, m: r, old: old, new: nw}
	g.addStandbyContainer(n.wFake, "rep1", r, label)
	pl := old
	if primaryNew {
		pl = nw
	}
	g.setPrimary(prodID, pl)
	n.ctlFake.ExecFn = func(cmd []string) string {
		if strings.Contains(strings.Join(cmd, " "), "current_setting") {
			return "max_connections|0|100|postmaster\nmax_worker_processes|0|8|postmaster\nmax_wal_senders|0|10|postmaster\nmax_prepared_transactions|0|0|postmaster\nmax_locks_per_transaction|0|64|postmaster\n"
		}
		return "0"
	}
	n.orch.probeRuns.Store(r.ID, 1)
	return g
}

// addStandbyContainer puts a running standby container of m on f: the
// settings label the spec has now, the old image, and the image label when
// label.
func (g *imageRig) addStandbyContainer(f *dockerfake.Fake, id string, m InstanceMember, label bool) {
	host, err := g.n.orch.memberHost(context.Background(), m, m.NodeID)
	if err != nil {
		panic(err)
	}
	spec, err := g.n.orch.engines.StandbySpec(context.Background(), g.t, m, host)
	if err != nil {
		panic(err)
	}
	lbl := map[string]string{LabelMember: m.ID, mflow.LabelConf: spec.Labels[mflow.LabelConf]}
	if label {
		lbl[mflow.LabelImage] = g.old
	}
	f.Containers = append(f.Containers, docker.Container{ID: id, Name: id, State: "running", Image: g.old, Labels: lbl})
}

// setPrimary says what the primary's container runs and what it was made for
// (the new label; "" = a container made before it).
func (g *imageRig) setPrimary(image, label string) {
	for i := range g.n.ctlFake.Containers {
		if c := &g.n.ctlFake.Containers[i]; c.ID == "pg" {
			c.Image = image
			if label != "" {
				c.Labels[mflow.LabelImage] = label
			}
		}
	}
}

// recreated says whether standby container id was removed and made again on
// the new image, with its data volume and the image label.
func (g *imageRig) recreated(f *dockerfake.Fake, id string, m InstanceMember) bool {
	if !sawCall(f, "StopRemove("+id+")") {
		return false
	}
	return slices.ContainsFunc(f.Specs, func(s docker.ContainerSpec) bool {
		return s.Image == g.new && s.Labels[mflow.LabelImage] == g.new &&
			slices.ContainsFunc(s.Volumes, func(v string) bool { return strings.HasPrefix(v, m.Volume+":") })
	})
}

func (g *imageRig) pass() { g.n.orch.syncStandbyConf(context.Background(), g.t) }

// A running standby is made again on the instance's new image once the
// primary runs on it, with no restart pending; the primary is as production
// makes it (an image ID, the tag only in its label).
func TestStandbyFollowsImage(t *testing.T) {
	t.Parallel()
	g := newImageRig(t, true, true)
	g.pass()
	if !g.recreated(g.n.wFake, "rep1", g.m) {
		t.Errorf("standby stayed on %s: %v", g.old, callNames(g.n.wFake))
	}
}

// A standby does not move to an image its primary is not on (the update
// failed there, or the primary predates the label).
func TestStandbyImageHeld(t *testing.T) {
	t.Parallel()
	for name, g := range map[string]*imageRig{
		"primary still on the old image": newImageRig(t, true, false),
		"primary without the label":      newImageRig(t, true, true),
	} {
		if name == "primary without the label" {
			for i := range g.n.ctlFake.Containers {
				delete(g.n.ctlFake.Containers[i].Labels, mflow.LabelImage)
			}
		}
		g.pass()
		if sawCall(g.n.wFake, "StopRemove(") {
			t.Errorf("%s: the standby was made again", name)
		}
	}
}

// A standby made before the image label existed is judged by its image.
func TestStandbyWithoutLabelFollowsImage(t *testing.T) {
	t.Parallel()
	g := newImageRig(t, false, true)
	g.pass()
	if !g.recreated(g.n.wFake, "rep1", g.m) {
		t.Errorf("unlabelled standby on the old image stayed: %v", callNames(g.n.wFake))
	}
	g = newImageRig(t, false, true)
	for i := range g.n.wFake.Containers {
		g.n.wFake.Containers[i].Image = g.new
	}
	g.pass()
	if sawCall(g.n.wFake, "StopRemove(") {
		t.Error("unlabelled standby already on the image was made again")
	}
}

// With two standbys on the old image a pass makes one, and the next waits
// until that one is seen streaming.
func TestStandbyImageOnePerPass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g := newImageRig(t, true, true)
	n := g.n
	must(t, n.st.Nodes.Create(ctx, store.Node{
		ID: "w2", Name: "w2", Slug: "w2", Role: node.Worker, Tags: "[]", RangeCIDR: "10.210.0.0/16",
		Status: node.StatusOnline, CreatedAt: time.Now(),
	}))
	w2 := dockerfake.New()
	n.orch.docker.(*DockerRouter).Set("w2", &subnetFake{Fake: w2, nets: map[string]bool{}})
	m2, err := n.orch.AddStandby(ctx, adminP(), n.devT.ID, "w2")
	must(t, err)
	n.orch.vipAsync.Wait()
	g.addStandbyContainer(w2, "rep2", m2, true)
	n.orch.probeRuns.Store(m2.ID, 1)

	creates := func() int {
		c := 0
		for _, f := range []*dockerfake.Fake{n.wFake, w2} {
			for _, call := range f.Calls() {
				if call.Method == "Create" && strings.Contains(strings.Join(call.Args, " "), g.new) {
					c++
				}
			}
		}
		return c
	}
	base := creates() // adding the second standby made its container on the new image
	g.pass()
	if creates()-base != 1 {
		t.Fatalf("first pass made %d standbys, want 1", creates()-base)
	}
	// the made one now runs the new image, and has not been probed yet
	first, firstID, second := g.m, "rep1", m2
	f1 := n.wFake
	if !sawCall(n.wFake, "StopRemove(rep1)") {
		first, firstID, second, f1 = m2, "rep2", g.m, w2
	}
	for i := range f1.Containers {
		if f1.Containers[i].ID == firstID {
			f1.Containers[i].Image = g.new
			f1.Containers[i].Labels[mflow.LabelImage] = g.new
		}
	}
	g.pass()
	if creates()-base != 1 {
		t.Fatalf("second pass made another while the first was not streaming: %d", creates()-base)
	}
	n.orch.probeRuns.Store(first.ID, 1)
	g.pass()
	if creates()-base != 2 {
		t.Errorf("second standby not made once the first streams: %d (%s)", creates()-base, second.ID)
	}
}

// A pull that fails leaves the old container running.
func TestStandbyImagePullFails(t *testing.T) {
	t.Parallel()
	g := newImageRig(t, true, true)
	g.n.wFake.Err = map[string]error{"Pull": errors.New("registry down")}
	g.pass()
	if sawCall(g.n.wFake, "StopRemove(") || sawCall(g.n.wFake, "Create(") {
		t.Errorf("old container touched: %v", callNames(g.n.wFake))
	}
}

// Nothing is made again while the instance fails over, or while the tile has
// a live job (an extensions change may still be rolled back).
func TestStandbyImageHeldByFailoverAndJob(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g := newImageRig(t, true, true)
	inst, err := g.n.orch.operated.GetByTile(ctx, g.t.ID)
	must(t, err)
	must(t, g.n.orch.saveFailover(ctx, inst.ID, failover{Kind: foPromoted, Step: stepStop, Started: time.Now()}))
	g.pass()
	if sawCall(g.n.wFake, "StopRemove(") {
		t.Error("made again during a failover")
	}
	must(t, g.n.orch.store.Settings.Delete(ctx, failoverKey(inst.ID)))
	j, err := g.n.orch.jobRows.Create(ctx, "deploy", []string{g.t.ID}, "{}", nil, t.TempDir())
	must(t, err)
	_, err = g.n.orch.jobRows.Start(ctx, j)
	must(t, err)
	g.pass()
	if sawCall(g.n.wFake, "StopRemove(") {
		t.Error("made again during a job")
	}
}

// The image changes again while the old one is pulled: under the lock the
// spec is rebuilt from the tile as it is, and the pass is skipped (the pulled
// image is outdated); once the primary runs on the newest one, the next pass
// makes the standby from it. The fake's pull has no hook, so the under-lock
// re-validation is tested directly.
func TestStandbyImageChangesDuringPull(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g := newImageRig(t, true, true)
	c := g.n.wFake.Containers[len(g.n.wFake.Containers)-1]
	if _, ok, err := g.n.orch.standbyStillDue(ctx, g.t, g.m, c, g.new); err != nil || !ok {
		t.Fatalf("unchanged tile: ok=%v err=%v", ok, err)
	}
	newer, ok := g.new+"-b", true
	if !ok {
		t.Skip("no newer minor known")
	}
	must(t, g.n.st.Tiles.SetImageRef(ctx, g.t.ID, newer))
	if _, ok, err := g.n.orch.standbyStillDue(ctx, g.t, g.m, c, g.new); err != nil || ok {
		t.Fatalf("outdated pulled image accepted: ok=%v err=%v", ok, err)
	}
	// the primary has not reached the newest image: nothing is made
	g.t, _ = g.n.orch.tiles.Get(ctx, g.t.ID)
	g.pass()
	if sawCall(g.n.wFake, "StopRemove(") {
		t.Error("made again before the primary runs the newest image")
	}
	g.setPrimary(prodID, newer)
	g.pass()
	if !sawCall(g.n.wFake, "StopRemove(rep1)") || !slices.ContainsFunc(g.n.wFake.Specs, func(s docker.ContainerSpec) bool { return s.Image == newer }) {
		t.Errorf("standby not made on %s: %v", newer, callNames(g.n.wFake))
	}
	if slices.ContainsFunc(g.n.wFake.Specs, func(s docker.ContainerSpec) bool { return s.Image == g.new }) {
		t.Error("standby made on the outdated image")
	}
}

// A stopped standby on an old image is made again at its start.
func TestStoppedStandbyFollowsImage(t *testing.T) {
	t.Parallel()
	g := newImageRig(t, true, true)
	for i := range g.n.wFake.Containers {
		if g.n.wFake.Containers[i].ID == "rep1" {
			g.n.wFake.Locked(func() { g.n.wFake.Containers[i].State = "exited" })
		}
	}
	must(t, g.n.orch.startMember(context.Background(), g.m))
	if !g.recreated(g.n.wFake, "rep1", g.m) {
		t.Errorf("stopped standby kept its image: %v", callNames(g.n.wFake))
	}
}

// staleSettings makes standby container id carry another settings label than
// its spec has now.
func staleSettings(f *dockerfake.Fake, id string) {
	for i := range f.Containers {
		if f.Containers[i].ID == id {
			f.Containers[i].Labels[mflow.LabelConf] = "old"
		}
	}
}

// pendingExec makes the standbys report a restart pending (and the primary
// the default limits).
func (g *imageRig) pendingExec() {
	g.n.ctlFake.ExecFn = func(cmd []string) string {
		c := strings.Join(cmd, " ")
		if strings.Contains(c, "current_setting") {
			return "max_connections|0|100|postmaster\nmax_worker_processes|0|8|postmaster\nmax_wal_senders|0|10|postmaster\nmax_prepared_transactions|0|0|postmaster\nmax_locks_per_transaction|0|64|postmaster\n"
		}
		return "1"
	}
	g.n.wFake.ExecFn = func([]string) string { return "1" }
}

// A container made with other settings AND from another image is made again
// at once through the settings path: the image guards (a live job, a primary
// not yet on the image) do not hold it back, running with a restart pending
// or stopped.
func TestStandbyDoubleStaleSettingsPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, stopped := range []bool{false, true} {
		g := newImageRig(t, true, false) // the primary is not on the new image
		g.pendingExec()
		staleSettings(g.n.wFake, "rep1")
		j, err := g.n.orch.jobRows.Create(ctx, "deploy", []string{g.t.ID}, "{}", nil, t.TempDir())
		must(t, err)
		_, err = g.n.orch.jobRows.Start(ctx, j)
		must(t, err)
		if stopped {
			for i := range g.n.wFake.Containers {
				g.n.wFake.Locked(func() { g.n.wFake.Containers[i].State = "exited" })
			}
			must(t, g.n.orch.startMember(ctx, g.m))
		} else {
			g.pass()
		}
		if !g.recreated(g.n.wFake, "rep1", g.m) {
			t.Errorf("stopped=%v: double-stale standby not made again: %v", stopped, callNames(g.n.wFake))
		}
	}
}

// A sibling standby on a node that is down does not count as streaming, even
// though its last probe was positive.
func TestStandbyImageHeldBySiblingOnDownNode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	g := newImageRig(t, true, true)
	n := g.n
	must(t, n.st.Nodes.Create(ctx, store.Node{
		ID: "w2", Name: "w2", Slug: "w2", Role: node.Worker, Tags: "[]", RangeCIDR: "10.210.0.0/16",
		Status: node.StatusOnline, CreatedAt: time.Now(),
	}))
	n.orch.docker.(*DockerRouter).Set("w2", &subnetFake{Fake: dockerfake.New(), nets: map[string]bool{}})
	m2, err := n.orch.AddStandby(ctx, adminP(), n.devT.ID, "w2")
	must(t, err)
	n.orch.vipAsync.Wait()
	n.orch.probeRuns.Store(m2.ID, 5)
	w2, err := n.st.Nodes.Get(ctx, "w2")
	must(t, err)
	w2.Status = node.StatusOffline
	must(t, n.st.Nodes.Update(ctx, w2))
	g.pass()
	if sawCall(n.wFake, "StopRemove(") {
		t.Errorf("made again with a sibling on a down node: %v", callNames(n.wFake))
	}
}

// The row is judged again under the lock: a member fenced, stopped (its
// container up), rebuilt or removed since is left alone. (The window is the
// image pull, which the harness cannot pause, so the check is tested alone.)
func TestStandbyEligible(t *testing.T) {
	t.Parallel()
	ok := InstanceMember{Role: opmember.Replica, State: opmember.Running}
	for name, c := range map[string]struct {
		m       InstanceMember
		running bool
		want    bool
	}{
		"running":                {ok, true, true},
		"stopped, to be started": {InstanceMember{Role: opmember.Replica, State: opmember.Stopped}, false, true},
		"stopped since":          {InstanceMember{Role: opmember.Replica, State: opmember.Stopped}, true, false},
		"fenced":                 {InstanceMember{Role: opmember.Replica, State: opmember.Running, Fenced: true}, true, false},
		"fenced, to be started":  {InstanceMember{Role: opmember.Replica, State: opmember.Stopped, Fenced: true}, false, false},
		"rebuilding":             {InstanceMember{Role: opmember.Replica, State: opmember.Running, Phase: opmember.Rebuild}, true, false},
		"removing":               {InstanceMember{Role: opmember.Replica, State: opmember.Removing}, true, false},
		"promoted":               {InstanceMember{Role: opmember.Primary, State: opmember.Running}, true, false},
	} {
		if got := standbyEligible(c.m, c.running); got != c.want {
			t.Errorf("%s: %v, want %v", name, got, c.want)
		}
	}
}

// A container made from a recipe-built image inherits that image's
// stackr.image label; it says nothing about the reference the container was
// made from, whose image is then judged.
func TestStandbyInheritedImageLabelIgnored(t *testing.T) {
	t.Parallel()
	g := newImageRig(t, false, true)
	for i := range g.n.wFake.Containers {
		c := &g.n.wFake.Containers[i]
		c.Image = g.new
		c.Labels["stackr.image"] = "pgext"
	}
	g.pass()
	if sawCall(g.n.wFake, "StopRemove(") {
		t.Errorf("standby on the right image made again: %v", callNames(g.n.wFake))
	}
}

// The primary's stamp, from the real deploy path, is the reference a standby
// of the same instance is made from, with and without an image ref set.
func TestPrimaryStampMatchesStandbyImage(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	for _, set := range []bool{false, true} {
		n := newTwoNodes(t)
		in := pgOn(t, n)
		_, err := n.orch.operated.SetMajor(ctx, in, mflow.ImageMajor(mflow.Engines["postgres"].Definition().Image))
		must(t, err)
		// the harness's tile is an image tile; the deploy stamps operated ones
		row, err := n.st.Tiles.Get(ctx, n.devT.ID)
		must(t, err)
		row.Kind = tile.Operated
		must(t, n.st.Tiles.Update(ctx, row))
		ref := ""
		if set {
			nw, ok := mflow.PGUpdate(mflow.Engines["postgres"].Definition().Image)
			if !ok {
				t.Skip("no newer minor known")
			}
			ref = nw
		}
		must(t, n.st.Tiles.SetImageRef(ctx, n.devT.ID, ref))
		r, err := n.orch.AddStandby(ctx, adminP(), n.devT.ID, "w1")
		must(t, err)
		n.orch.vipAsync.Wait()
		tl, _ := n.orch.tiles.Get(ctx, n.devT.ID)
		host, err := n.orch.memberHost(ctx, r, r.NodeID)
		must(t, err)
		sb, err := n.orch.engines.StandbySpec(ctx, tl, r, host)
		must(t, err)
		n.ctlFake.Specs = nil
		_ = n.orch.deploy.Redeploy(docker.WithNode(ctx, n.ctl.ID), tl.ID, io.Discard, nil) // the fake container fails the health gate; its spec is made by then
		if len(n.ctlFake.Specs) == 0 {
			t.Fatalf("set=%v: the redeploy made no container", set)
		}
		got := n.ctlFake.Specs[len(n.ctlFake.Specs)-1].Labels[mflow.LabelImage]
		if got == "" || got != sb.Image {
			t.Errorf("set=%v: primary stamp %q, standby image %q", set, got, sb.Image)
		}
	}
}
