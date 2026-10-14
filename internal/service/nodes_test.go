package service

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/internal/storetest"
	"github.com/FyrmForge/stackr/internal/service/internal/vip"
)

// A Docker call with no node in its context fails loudly, and touches no
// daemon: every caller has to decide which node it means.
func TestRouterNeedsANode(t *testing.T) {
	a := dockerfake.New()
	r := NewRouter(map[string]Docker{"n1": a})
	ctx := context.Background()

	if _, err := r.List(ctx, nil); !errors.Is(err, docker.ErrNoNode) {
		t.Fatalf("List without a node = %v, want ErrNoNode", err)
	}
	if err := r.EnsureNetwork(ctx, "net", netip.Prefix{}, nil); !errors.Is(err, docker.ErrNoNode) {
		t.Fatalf("EnsureNetwork without a node = %v, want ErrNoNode", err)
	}
	if _, _, err := r.MemberAddr(ctx, "net", "c"); !errors.Is(err, docker.ErrNoNode) {
		t.Fatalf("MemberAddr without a node = %v, want ErrNoNode", err)
	}
	if _, _, _, err := r.ExecTTY(ctx, "c", nil); !errors.Is(err, docker.ErrNoNode) {
		t.Fatalf("ExecTTY without a node = %v, want ErrNoNode", err)
	}
	if got := a.Calls(); len(got) != 0 {
		t.Fatalf("a daemon was called without a node: %v", got)
	}

	// A node with no connection is unavailable (503), not a bug.
	if _, err := r.List(WithNode(ctx, "gone"), nil); !errors.Is(err, errs.ErrBusy) {
		t.Fatalf("List on an unknown node = %v, want ErrBusy", err)
	}
}

// A node's tool image is made pullable first (the registry helper on a
// worker); a failure stops the tool call before it reaches the node.
func TestRouterToolReady(t *testing.T) {
	a := dockerfake.New()
	r := NewRouter(map[string]Docker{"w1": a})
	var asked []string
	r.ToolReady = func(_ context.Context, id string) error {
		asked = append(asked, id)
		return nil
	}
	ctx := WithNode(context.Background(), "w1")
	must(t, r.EnsureTool(ctx))
	if !slices.Equal(asked, []string{"w1"}) || len(a.Calls()) != 1 {
		t.Fatalf("asked %v, calls %v", asked, a.Calls())
	}
	r.ToolReady = func(context.Context, string) error { return errors.New("no registry") }
	if err := r.EnsureTool(ctx); err == nil || len(a.Calls()) != 1 {
		t.Fatalf("err %v, calls %v", err, a.Calls())
	}
}

// Each call goes to the client of the node in its context, and only there.
func TestRouterPicksTheNodesClient(t *testing.T) {
	a, b := dockerfake.New(), dockerfake.New()
	r := NewRouter(map[string]Docker{"n1": a})
	r.Set("n2", b)
	ctx := context.Background()

	must(t, r.Start(WithNode(ctx, "n1"), "c1"))
	must(t, r.Stop(WithNode(ctx, "n2"), "c2"))
	if got := callNames(a); !slices.Equal(got, []string{"Start(c1)"}) {
		t.Errorf("n1 saw %v", got)
	}
	if got := callNames(b); !slices.Equal(got, []string{"Stop(c2)"}) {
		t.Errorf("n2 saw %v", got)
	}
	r.Remove("n2")
	if err := r.Stop(WithNode(ctx, "n2"), "c2"); !errors.Is(err, errs.ErrBusy) {
		t.Errorf("a removed node's call = %v, want ErrBusy", err)
	}
}

func callNames(f *dockerfake.Fake) []string {
	var out []string
	for _, c := range f.Calls() {
		out = append(out, c.String())
	}
	return out
}

func sawCall(f *dockerfake.Fake, prefix string) bool {
	return slices.ContainsFunc(f.Calls(), func(c dockerfake.Call) bool { return strings.HasPrefix(c.String(), prefix) })
}

// A tar backup of a volume runs on the volume's node, through the router:
// the tool pull and the tar reach that node's daemon, the other's is never
// asked. A pull without the node is refused by the router with ErrNoNode.
func TestTarBackupRunsOnVolumeNode(t *testing.T) {
	n := newTwoNodes(t)
	ctx := context.Background()
	var buf bytes.Buffer // an empty tar, gzipped: what the helper would stream
	gz := gzip.NewWriter(&buf)
	must(t, tar.NewWriter(gz).Close())
	must(t, gz.Close())
	n.ctlFake.TarOut, n.wFake.TarOut = buf.Bytes(), buf.Bytes()
	for _, c := range []struct {
		env         Environment
		mine, other *dockerfake.Fake
	}{{n.dev, n.ctlFake, n.wFake}, {n.edge, n.wFake, n.ctlFake}} {
		v, err := n.orch.DeclareVolume(ctx, VolumeScope{Kind: "env", ID: c.env.ID}, "data-"+c.env.Slug, 0)
		must(t, err)
		j, err := n.orch.BackupNow(ctx, v.ID, "", "volume", "live")
		must(t, err)
		if j = n.wait(t, j); j.State != job.Done {
			t.Fatalf("%s backup = %s %q", c.env.Slug, j.State, j.Error)
		}
		if !sawCall(c.mine, "EnsureTool") || !sawCall(c.mine, "TarVolume("+v.Name+")") {
			t.Errorf("%s: the node's daemon missed the tar: %v", c.env.Slug, callNames(c.mine))
		}
		if sawCall(c.other, "TarVolume("+v.Name+")") {
			t.Errorf("%s: the tar reached the other node", c.env.Slug)
		}
		// The restore's pre-restore backup and untar run there too.
		runs, err := n.orch.BackupRuns(ctx, v.ID)
		must(t, err)
		j, err = n.orch.RestoreBackup(ctx, nil, runs[0].ID, v.ID, v.ID)
		must(t, err)
		if j = n.wait(t, j); j.State != job.Done {
			t.Fatalf("%s restore = %s %q", c.env.Slug, j.State, j.Error)
		}
		if !sawCall(c.mine, "UntarVolume("+v.Name+")") || sawCall(c.other, "UntarVolume("+v.Name+")") {
			t.Errorf("%s: the untar did not stay on the node: %v", c.env.Slug, callNames(c.mine))
		}
	}
}

// The VIP table of a node is the one its calls reach; no node is an error.
func TestNodeVIPsRouteByNode(t *testing.T) {
	a, b := &recVIP{}, &recVIP{}
	v := newNodeVIPs(map[string]tile.VIP{"n1": a})
	v.Attach("n2", b)
	ctx := context.Background()

	must(t, v.Set(WithNode(ctx, "n1"), "10.213.1.2", []string{"10.213.1.3"}, nil))
	must(t, v.Remove(WithNode(ctx, "n2"), "10.209.1.2"))
	if !slices.Equal(a.set, []string{"10.213.1.2"}) || len(a.removed) != 0 {
		t.Errorf("n1 table: %+v", a)
	}
	if !slices.Equal(b.removed, []string{"10.209.1.2"}) || len(b.set) != 0 {
		t.Errorf("n2 table: %+v", b)
	}
	if err := v.Set(ctx, "10.213.1.2", nil, nil); !errors.Is(err, docker.ErrNoNode) {
		t.Errorf("Set without a node = %v", err)
	}
	if err := v.Remove(WithNode(ctx, "gone"), "10.213.1.2"); !errors.Is(err, errs.ErrBusy) {
		t.Errorf("Remove on an unknown node = %v", err)
	}
}

type recVIP struct{ set, removed []string }

func (r *recVIP) Set(_ context.Context, v string, _, _ []string) error {
	r.set = append(r.set, v)
	return nil
}

func (r *recVIP) Remove(_ context.Context, v string) error {
	r.removed = append(r.removed, v)
	return nil
}

// twoNodes is a panel with a controller and a worker, each on its own fake
// daemon behind the router: a stack with a dev env on the controller and an
// edge env on the worker, a tile in each, and a VIP table per node.
type twoNodes struct {
	orch        *Orchestrator
	st          *store.Store
	ctl         store.Node
	ctlFake     *dockerfake.Fake
	wFake       *dockerfake.Fake
	stack       string
	dev, edge   Environment
	devT, edgeT Tile
	ctlRS, wRS  *[]string // the scripts each node's table applied
	dir         string
}

func newTwoNodes(t *testing.T) *twoNodes {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	cfg := Config{DataDir: dir, SecretsKey: testKey, Conntrack: dir + "/nf_conntrack"}
	proxy := WithProxy(func(context.Context, json.RawMessage) error { return nil })

	// One boot to migrate and make the controller row, so the router can name it.
	first, err := New(cfg, WithDocker(dockerfake.New()), WithVIP(vipStub{}), proxy)
	must(t, err)
	must(t, first.Close())
	st := storetest.Open(t, filepath.Join(dir, "stackr.db"))
	ctl := storetest.Controller(t, st)
	must(t, st.Nodes.Create(ctx, store.Node{
		ID: "w1", Name: "w1", Slug: "w1", Role: node.Worker, Tags: "[]", RangeCIDR: "10.209.0.0/16",
		Status: node.StatusOnline, CreatedAt: time.Now(),
	}))

	n := &twoNodes{st: st, ctl: ctl, ctlFake: dockerfake.New(), wFake: dockerfake.New(), dir: dir}
	router := NewRouter(map[string]Docker{ctl.ID: n.ctlFake, "w1": n.wFake})
	n.orch, err = New(cfg, WithDocker(router), WithVIP(vipStub{}), proxy)
	must(t, err)
	t.Cleanup(func() { _ = n.orch.Close() })
	n.orch.envs.WithPlacement(n.orch.envPins)                       // service.New wires this; saying it twice is harmless
	n.orch.helperReady = func(context.Context) error { return nil } // no cluster registry here
	n.orch.tiles.Gate = tile.Gate{Poll: 5 * time.Millisecond, Grace: 10 * time.Millisecond, Deadline: time.Second}

	table := func(rs *[]string) *vip.Table {
		tb := vip.New()
		tb.Restore = func(_ context.Context, s string) error { *rs = append(*rs, s); return nil }
		tb.Exec = func(_ context.Context, argv ...string) error { // -D finds nothing, like iptables with no jump
			if slices.Contains(argv, "-D") {
				return context.Canceled
			}
			return nil
		}
		tb.Read = func(context.Context, ...string) (string, error) { return "", nil }
		return tb
	}
	if nv, ok := n.orch.vip.(*nodeVIPs); ok { // the tile leaf still holds this one: a worker table for it too
		nv.Attach("w1", vipStub{})
	}
	n.ctlRS, n.wRS = &[]string{}, &[]string{}
	n.orch.vip = newNodeVIPs(map[string]tile.VIP{ctl.ID: table(n.ctlRS), "w1": table(n.wRS)})

	org, stack := uuid.NewString(), uuid.NewString()
	now := time.Now()
	must(t, st.Orgs.Create(ctx, store.Org{ID: org, Name: "acme", Slug: "acme", EnvColors: "{}", Settings: "{}", CreatedAt: now}))
	must(t, st.Stacks.Create(ctx, store.Stack{ID: stack, OrgID: org, Name: "shop", Slug: "shop", Settings: "{}", CreatedAt: now}))
	n.stack = stack
	spec := func(node string) EnvSpec {
		return EnvSpec{NodeID: node, Type: environment.Static, FromKind: environment.FromBranch, FromBranch: "main"}
	}
	n.dev, err = n.orch.CreateEnv(ctx, stack, "dev", spec(""))
	must(t, err)
	n.edge, err = n.orch.CreateEnv(ctx, stack, "edge", spec("w1"))
	must(t, err)
	if n.dev.NodeID != ctl.ID || n.edge.NodeID != "w1" {
		t.Fatalf("envs placed on %q and %q; want the controller %q and w1", n.dev.NodeID, n.edge.NodeID, ctl.ID)
	}
	n.devT = n.tile(t, n.dev)
	n.edgeT = n.tile(t, n.edge)
	return n
}

func (n *twoNodes) tile(t *testing.T, env Environment) Tile {
	t.Helper()
	tl, err := n.orch.CreateTile(context.Background(), Tile{
		StackID: n.stack, EnvironmentID: env.ID, Name: "web-" + env.Slug, Kind: tile.Image,
		ImageRef: "nginx:1", ContainerPort: 80,
	})
	must(t, err)
	return tl
}

// wait polls a job until it finishes.
func (n *twoNodes) wait(t *testing.T, j Job) Job {
	t.Helper()
	for range 500 {
		var err error
		if j, err = n.orch.GetJob(context.Background(), j.ID); err != nil {
			t.Fatal(err)
		}
		if j.State == job.Done || j.State == job.Failed || j.State == job.Cancelled {
			return j
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("job %s never finished", j.ID)
	return j
}

// An env created on a node deploys there: its network and containers are on
// that node's daemon and the other's is never asked.
func TestEnvRunsOnItsNode(t *testing.T) {
	n := newTwoNodes(t)
	for _, c := range []struct {
		tl   Tile
		env  Environment
		f    *dockerfake.Fake
		skip *dockerfake.Fake
	}{{n.edgeT, n.edge, n.wFake, n.ctlFake}, {n.devT, n.dev, n.ctlFake, n.wFake}} {
		c.f.RunID = "c-" + c.env.Slug
		c.f.Details = map[string]docker.Detail{c.f.RunID: {Running: true, Networks: map[string]string{c.env.Network: "10.0.0.2"}}}
		j, err := n.orch.Deploy(context.Background(), c.tl.ID)
		must(t, err)
		if j = n.wait(t, j); j.State == job.Failed && strings.Contains(j.Error, docker.ErrNoNode.Error()) {
			t.Fatalf("deploy of %s lost its node: %s", c.env.Slug, j.Error)
		}
		if !sawCall(c.f, "EnsureNetwork("+c.env.Network+")") || !sawCall(c.f, "Run(") {
			t.Errorf("%s did not deploy on its node: %v", c.env.Slug, callNames(c.f))
		}
		if sawCall(c.skip, "EnsureNetwork("+c.env.Network+")") {
			t.Errorf("%s's network was made on the other node: %v", c.env.Slug, callNames(c.skip))
		}
	}
}

// Reads and verbs on a tile of the worker env reach the worker's daemon:
// status, logs, following a log, a restart job.
func TestTileVerbsReachTheEnvsNode(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	n.wFake.Containers = []docker.Container{{
		ID: "c-edge", Name: "c-edge", State: "running", Image: "nginx:1",
		Labels: map[string]string{tile.LabelTile: n.edgeT.ID, tile.LabelRole: "replica"},
	}}
	n.wFake.Details = map[string]docker.Detail{"c-edge": {Running: true, Networks: map[string]string{n.edge.Network: "10.209.0.2"}}}
	n.wFake.LogsOut = "hello from the worker\n"

	s, err := n.orch.TileStatus(ctx, n.edgeT.ID)
	if err != nil || s.Word != "running" || len(s.Replicas) != 1 {
		t.Fatalf("TileStatus = %+v, %v; want the worker's replica", s, err)
	}
	if got, err := n.orch.Logs(ctx, n.edgeT.ID, "", 10); err != nil || !strings.Contains(got, "hello from the worker") {
		t.Fatalf("Logs = %q, %v", got, err)
	}
	_, stop, err := n.orch.FollowLogs(ctx, n.edgeT.ID, "c-edge", 5)
	must(t, err)
	stop()
	j, err := n.orch.RestartTile(ctx, n.edgeT.ID)
	must(t, err)
	if j = n.wait(t, j); j.State != job.Done {
		t.Fatalf("restart = %s %q", j.State, j.Error)
	}
	if !sawCall(n.wFake, "Restart(c-edge)") {
		t.Errorf("the restart did not reach the worker: %v", callNames(n.wFake))
	}
	for _, c := range callNames(n.ctlFake) { // the controller was asked about nothing of this tile
		if strings.Contains(c, "c-edge") {
			t.Errorf("the controller saw the worker's container: %s", c)
		}
	}
}

// A cleanup sweep prunes every node, not only the one it was queued from; a
// node that fails leaves the others' work, and its own images, for the next.
func TestCleanupPrunesEveryNode(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	must(t, n.orch.SetSetting(ctx, "cleanup_enabled", "true"))
	sweep := func() Job {
		t.Helper()
		j, err := n.orch.enqueue(ctx, kindCleanup, nil, "cleanup")
		must(t, err)
		return n.wait(t, j)
	}
	if j := sweep(); j.State != job.Done {
		t.Fatalf("cleanup = %s %q", j.State, j.Error)
	}
	for name, f := range map[string]*dockerfake.Fake{"controller": n.ctlFake, "w1": n.wFake} {
		for _, m := range []string{"PruneDangling(", "PruneImages(", "BuildCachePrune("} {
			if !sawCall(f, m) {
				t.Errorf("%s was not asked for %s: %v", name, m, callNames(f))
			}
		}
	}

	old := time.Now().Add(-48 * time.Hour)
	must(t, n.st.Images.Create(ctx, store.Image{
		ID: "img1", Ref: "stkr/gone:1", Digest: "sha256:gone", BuiltAt: &old, CreatedAt: old,
	}))
	n.ctlFake.Images = []docker.Image{{ID: "sha256:gone", Tags: []string{"stkr/gone:1"}, Labels: map[string]string{"stackr.built": "true"}}}
	n.wFake.Err = map[string]error{"PruneImages": errors.New("daemon down")}
	n.wFake.Images = []docker.Image{{ID: "sha256:held", Tags: []string{"stkr/held:1"}, Labels: map[string]string{"stackr.built": "true"}}}
	if j := sweep(); j.State != job.Failed {
		t.Errorf("a node failing its prune left the sweep %s", j.State)
	}
	// The row of what the other node removed goes now; the worker's own image
	// has no row to keep it, so its next sweep prunes it.
	if _, err := n.st.Images.Get(ctx, "img1"); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("the image row stayed after a node removed the image: %v", err)
	}
	n.wFake.Err = nil
	if j := sweep(); j.State != job.Done {
		t.Fatalf("cleanup = %s %q", j.State, j.Error)
	}
	if len(n.wFake.Images) != 0 {
		t.Errorf("the worker still holds %v after a clean sweep", n.wFake.Images)
	}
}

// The VIP rebuild declares each node's rules on its own table, from its own
// daemon's pause containers, with its own networks in the filter.
func TestRebuildVIPsPerNode(t *testing.T) {
	n := newTwoNodes(t)
	ctx := context.Background()
	must(t, n.st.Environments.SetSubnet(ctx, n.dev.ID, "10.208.5.0/24"))
	must(t, n.st.Environments.SetSubnet(ctx, n.edge.ID, "10.208.6.0/24"))
	pause := func(f *dockerfake.Fake, tl Tile, net, vipIP, repIP string) {
		f.Containers = append(f.Containers,
			docker.Container{ID: "p-" + tl.Slug, State: "running", Labels: map[string]string{tile.LabelTile: tl.ID, tile.LabelRole: "pause"}},
			docker.Container{ID: "r-" + tl.Slug, State: "running", Labels: map[string]string{tile.LabelTile: tl.ID, tile.LabelRole: "replica"}})
		f.Details = map[string]docker.Detail{
			"p-" + tl.Slug: {Running: true, Networks: map[string]string{net: vipIP}},
			"r-" + tl.Slug: {Running: true, Networks: map[string]string{net: repIP}},
		}
	}
	pause(n.ctlFake, n.devT, n.dev.Network, "10.213.1.2", "10.213.1.3")
	pause(n.wFake, n.edgeT, n.edge.Network, "10.209.1.2", "10.209.1.3")
	must(t, n.orch.rebuildVIPs(context.Background()))
	if len(*n.ctlRS) != 1 || len(*n.wRS) != 1 {
		t.Fatalf("restores: controller %d, w1 %d; want one each", len(*n.ctlRS), len(*n.wRS))
	}
	c, w := (*n.ctlRS)[0], (*n.wRS)[0]
	for _, want := range []string{"-d 10.213.1.2/32", "-s 10.208.5.0/24 -j STACKR-FWD-OWN", "-d 10.208.0.0/12 -j DROP"} {
		if !strings.Contains(c, want) {
			t.Errorf("the controller's rules lack %q:\n%s", want, c)
		}
	}
	for _, want := range []string{"-d 10.209.1.2/32", "-s 10.208.6.0/24 -j STACKR-FWD-OWN", "-d 10.208.0.0/12 -j DROP"} {
		if !strings.Contains(w, want) {
			t.Errorf("w1's rules lack %q:\n%s", want, w)
		}
	}
	if strings.Contains(c, "10.209.1.") || strings.Contains(w, "10.213.1.") || strings.Contains(c, "10.208.6.") || strings.Contains(w, "10.208.5.") {
		t.Errorf("one node's rules carry the other's tiles:\ncontroller:\n%s\nw1:\n%s", c, w)
	}
}

// The scheduler's traffic sample is controller work: no caller names a node
// for it, and the router still finds one.
func TestTrafficTickNeedsNoCallerNode(t *testing.T) {
	n := newTwoNodes(t)
	must(t, os.WriteFile(filepath.Join(n.dir, "nf_conntrack"), nil, 0o600))
	if err := n.orch.trafficTick(context.Background()); err != nil {
		t.Fatalf("trafficTick = %v", err)
	}
	if !sawCall(n.ctlFake, "Inspect(stackr-proxy)") {
		t.Errorf("the sample did not ask the controller about the proxy: %v", callNames(n.ctlFake))
	}
	if sawCall(n.wFake, "Inspect(stackr-proxy)") {
		t.Errorf("the sample asked the worker about the proxy")
	}
}

// Once calls are routed, an env row with no node is an error, not a guess; a
// panel that does not route runs it on the controller, as before nodes.
func TestUnplacedEnv(t *testing.T) {
	ctx := context.Background()
	routed := newTwoNodes(t)
	bare := store.Environment{ID: uuid.NewString(), StackID: routed.stack, Name: "old", Slug: "old", Type: "static", Settings: "{}",
		Network: "old", FromKind: "branch", FromBranch: "main", CreatedAt: time.Now()}
	must(t, routed.st.Environments.Create(ctx, bare))
	if _, err := routed.orch.withEnvNode(ctx, bare); !errors.Is(err, docker.ErrNoNode) {
		t.Errorf("routed, an unplaced env = %v, want ErrNoNode", err)
	}
	if _, err := routed.orch.envNode(ctx, bare.ID); !errors.Is(err, docker.ErrNoNode) {
		t.Errorf("routed, envNode of an unplaced env = %v, want ErrNoNode", err)
	}

	w := newWorld(t) // WithDocker(fake): nothing routes, until service.New wires the router itself
	if _, routed := w.orch.docker.(*DockerRouter); routed {
		return
	}
	bare.ID, bare.StackID, bare.Slug = uuid.NewString(), w.stack, "old"
	got, err := w.orch.withEnvNode(ctx, bare)
	if id, ok := docker.NodeFrom(got); err != nil || !ok || id != ctlNode(t, w.orch).ID {
		t.Errorf("unrouted, an unplaced env: node %q, %v; want the controller", id, err)
	}
}

// The hooks the tile and volume leaves are given: an env's own node, the
// controller for what has none (a stack, an org, a share).
func TestEnvAndScopeNode(t *testing.T) {
	ctx := context.Background()
	w := newWorld(t)
	ctl := ctlNode(t, w.orch)
	must(t, w.st.Nodes.Create(ctx, store.Node{
		ID: "w1", Name: "w1", Slug: "w1", Role: node.Worker, Tags: "[]", RangeCIDR: "10.209.0.0/16",
		Status: node.StatusOnline, CreatedAt: time.Now(),
	}))
	other := uuid.NewString()
	must(t, w.st.Environments.Create(ctx, store.Environment{
		ID: other, StackID: w.stack, Name: "edge", Slug: "edge", Type: "static", Settings: "{}", Network: "m",
		FromKind: "branch", FromBranch: "main", CreatedAt: time.Now(), NodeID: "w1",
	}))
	for name, tc := range map[string]struct{ kind, id, want string }{
		"env on the controller": {"env", w.env, ctl.ID},
		"env on w1":             {"env", other, "w1"},
		"stack":                 {"stack", w.stack, ctl.ID},
		"org":                   {"org", w.org, ctl.ID},
		"share":                 {"", "", ctl.ID},
	} {
		got, err := w.orch.scopeNode(ctx, tc.kind, tc.id)
		if err != nil || got != tc.want {
			t.Errorf("%s: node %q, %v; want %q", name, got, err, tc.want)
		}
	}
	if got, err := w.orch.envNode(ctx, ""); err != nil || got != ctl.ID {
		t.Errorf("no env: node %q, %v; want the controller", got, err)
	}
	if _, err := w.orch.envNode(ctx, uuid.NewString()); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("a missing env = %v, want ErrNotFound", err)
	}
}

// An image of stackr's own registry is pulled from localhost:5000 on the
// node (the worker's front over the mesh) and tagged as named; others as is.
func TestRouterPullsOwnRegistryLocally(t *testing.T) {
	a := dockerfake.New()
	r := NewRouter(map[string]Docker{"n1": a})
	r.OwnRegistry = func(context.Context) string { return "registry.stkr.example.com" }
	ctx := WithNode(context.Background(), "n1")

	if err := r.Pull(ctx, "registry.stkr.example.com/acme/web/api:b1", "", nil); err != nil {
		t.Fatal(err)
	}
	if err := r.Pull(ctx, "nginx:1.27", "", nil); err != nil {
		t.Fatal(err)
	}
	// A digest ref cannot be tagged: it is pulled and used by its local name.
	pinned := "registry.stkr.example.com/acme/web/api@sha256:aa"
	if err := r.Pull(ctx, pinned, "", nil); err != nil {
		t.Fatal(err)
	}
	if got := r.pinnedLocal(ctx, pinned); got != "localhost:5000/acme/web/api@sha256:aa" {
		t.Fatalf("pinnedLocal = %s", got)
	}
	if got := r.pinnedLocal(ctx, "registry.stkr.example.com/acme/web/api:b1"); got != "registry.stkr.example.com/acme/web/api:b1" {
		t.Fatalf("tag ref rewritten: %s", got)
	}
	var got []string
	for _, c := range a.Calls() {
		got = append(got, c.String())
	}
	want := []string{
		"Pull(localhost:5000/acme/web/api:b1)",
		"Tag(localhost:5000/acme/web/api:b1, registry.stkr.example.com/acme/web/api:b1)",
		"Pull(nginx:1.27)",
		"Pull(localhost:5000/acme/web/api@sha256:aa)",
	}
	if !slices.Equal(got, want) {
		t.Fatalf("calls = %v, want %v", got, want)
	}
}
