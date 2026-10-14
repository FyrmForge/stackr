package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/jobs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

const testImage = "localhost:5000/stackr.system/stackr:9.9.9"

// rolloutRig is a controller with three more workers, each its own fake
// daemon, and a rollout whose facts and image are scripted.
type rolloutRig struct {
	n     *twoNodes
	fakes map[string]*dockerfake.Fake
	facts map[string]func() *worker.Facts // nil func or nil result = unreachable
	ro    rollout
}

func newRolloutRig(t *testing.T, names ...string) *rolloutRig {
	t.Helper()
	n := newTwoNodes(t)
	n.orch.cfg.Version = "v9.9.9"
	ctx := context.Background()
	r := &rolloutRig{n: n, fakes: map[string]*dockerfake.Fake{n.ctl.ID: n.ctlFake}, facts: map[string]func() *worker.Facts{}}
	// twoNodes already made w1; the rig replaces its fake so all share one shape.
	all := append([]string{"w1"}, names...)
	for i, id := range all {
		if id != "w1" {
			must(t, n.st.Nodes.Create(ctx, store.Node{
				ID: id, Name: id, Slug: id, Role: node.Worker, Tags: "[]", RangeCIDR: fmt.Sprintf("10.%d.0.0/16", 210+i),
				Status: node.StatusOnline, CreatedAt: time.Now(),
			}))
		}
		f := dockerfake.New()
		f.RunID = "helper-" + id
		r.fakes[id] = f
		r.facts[id] = func() *worker.Facts { return &worker.Facts{Version: "v9.8.0", Arch: "amd64"} }
	}
	docks := map[string]Docker{}
	for id, f := range r.fakes {
		docks[id] = f
	}
	r.ro = rollout{
		o:        n.orch,
		docker:   NewRouter(docks),
		image:    func(context.Context) (string, error) { return testImage, nil },
		hops:     func(context.Context) map[string]bool { return nil },
		allowOld: func(ctx context.Context) context.Context { return ctx },
		facts: func(_ context.Context, id string) (*worker.Facts, error) {
			if f := r.facts[id]; f != nil {
				if got := f(); got != nil {
					return got, nil
				}
			}
			return nil, errors.New("unreachable")
		},
		arch:     "amd64",
		interval: 2 * time.Millisecond,
		wait:     60 * time.Millisecond,
		exitWait: 20 * time.Millisecond,
		log:      io.Discard,
	}
	return r
}

func (r *rolloutRig) state(t *testing.T, id string) nodeUpgrade {
	t.Helper()
	return r.ro.o.upgradeOf(context.Background(), id)
}

func (r *rolloutRig) setting(key string) string {
	v, _, _ := r.n.orch.store.Settings.Get(context.Background(), key)
	return v
}

// The nodes others are reached through go last, then by name.
func TestRolloutOrder(t *testing.T) {
	ns := []store.Node{{ID: "a", Name: "vm106"}, {ID: "b", Name: "h4"}, {ID: "c", Name: "ct110"}, {ID: "d", Name: "h3"}}
	got := rolloutOrder(ns, map[string]bool{"b": true})
	var names []string
	for _, n := range got {
		names = append(names, n.Name)
	}
	if want := []string{"ct110", "h3", "vm106", "h4"}; !slices.Equal(names, want) {
		t.Fatalf("order = %v, want %v", names, want)
	}
}

// An offline node is skipped with a reason; the rest still go.
func TestRolloutSkipsOffline(t *testing.T) {
	r := newRolloutRig(t, "w2")
	ctx := context.Background()
	w1, err := r.n.st.Nodes.Get(ctx, "w1")
	must(t, err)
	w1.Status = node.StatusOffline
	must(t, r.n.st.Nodes.Update(ctx, w1))
	r.facts["w2"] = r.upgradeOnRun("w2")

	if err := r.ro.run(ctx); err != nil {
		t.Fatal(err)
	}
	if s := r.state(t, "w1"); s.State != stateSkipped || s.Error == "" {
		t.Fatalf("offline node = %+v, want skipped with a reason", s)
	}
	if s := r.state(t, "w2"); s.State != stateDone {
		t.Fatalf("w2 = %+v, want done", s)
	}
	if len(r.fakes["w1"].Specs) != 0 {
		t.Fatal("a helper ran on an offline node")
	}
}

// upgradeOnRun reports the old version until the helper ran on id, then the new one.
func (r *rolloutRig) upgradeOnRun(id string) func() *worker.Facts {
	return func() *worker.Facts {
		v := "v9.8.0"
		if len(r.fakes[id].Specs) > 0 {
			v = "9.9.9" // no v: the comparison ignores it
		}
		return &worker.Facts{Version: v, Arch: "amd64"}
	}
}

// A worker of another CPU type is refused with a reason naming both; the
// rollout goes on.
func TestRolloutRefusesOtherArch(t *testing.T) {
	r := newRolloutRig(t, "w2")
	r.facts["w1"] = func() *worker.Facts { return &worker.Facts{Version: "v9.8.0", Arch: "arm64"} }
	r.facts["w2"] = r.upgradeOnRun("w2")
	if err := r.ro.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	s := r.state(t, "w1")
	if s.State != stateSkipped || s.Error != "w1 is arm64; the stackr image is amd64 only" {
		t.Fatalf("arm64 node = %+v", s)
	}
	if len(r.fakes["w1"].Specs) != 0 {
		t.Fatal("a helper ran on a node of the wrong arch")
	}
	if r.state(t, "w2").State != stateDone {
		t.Fatal("the rollout stopped at a refused node")
	}
}

// The helper is the contract's: privileged, host pid and network, / at /host,
// the swap command; it is removed once the node reports the new version, and
// the node is done. A node already current is left alone.
func TestRolloutRunsHelperAndCommits(t *testing.T) {
	r := newRolloutRig(t, "w2")
	r.facts["w1"] = r.upgradeOnRun("w1")
	r.facts["w2"] = func() *worker.Facts { return &worker.Facts{Version: "v9.9.9", Arch: "amd64"} }
	r.fakes["w1"].Containers = []docker.Container{{ID: "stale", Name: hostHelperName, Labels: map[string]string{"stackr.role": "host-helper"}}}

	if err := r.ro.run(context.Background()); err != nil {
		t.Fatal(err)
	}
	f := r.fakes["w1"]
	if len(f.Specs) != 1 {
		t.Fatalf("helper runs = %d, want 1", len(f.Specs))
	}
	s := f.Specs[0]
	if s.Name != "stackr-host-helper" || s.Image != testImage || !s.Privileged || !s.PidHost || !s.HostNetwork ||
		!slices.Equal(s.Volumes, []string{"/:/host"}) || s.Restart != "no" || s.Labels["stackr.role"] != "host-helper" ||
		!slices.Equal(s.Cmd, []string{"worker-swap", "--deadline", "150s"}) {
		t.Fatalf("helper spec = %+v", s)
	}
	names := callNames(f)
	if !slices.Contains(names, "Pull("+testImage+")") && !sawCall(f, "Pull") {
		t.Fatalf("no pull: %v", names)
	}
	if !sawCall(f, "StopRemove(stale") || !sawCall(f, "StopRemove(helper-w1") {
		t.Fatalf("leftover or finished helper not removed: %v", names)
	}
	if got := r.state(t, "w1"); got.State != stateDone || got.Version != "v9.9.9" {
		t.Fatalf("w1 = %+v", got)
	}
	if got := r.state(t, "w2"); got.State != stateDone || len(r.fakes["w2"].Specs) != 0 {
		t.Fatalf("a current node was touched: %+v %v", got, r.fakes["w2"].Specs)
	}
	if r.setting(keyRolloutStopped) != "" {
		t.Fatal("a clean rollout stopped")
	}
}

// A node that never shows the new version is failed after the helper rolled
// it back; the rollout stops, later nodes stay waiting.
func TestRolloutStopsWhenTheVersionNeverShows(t *testing.T) {
	r := newRolloutRig(t, "w2") // w1 sorts first, w2 after
	err := r.ro.run(context.Background())
	if err == nil {
		t.Fatal("want the failure")
	}
	if got := r.state(t, "w1"); got.State != stateFailed || got.Error == "" {
		t.Fatalf("w1 = %+v", got)
	}
	if got := r.state(t, "w2"); got.State != stateWaiting {
		t.Fatalf("w2 = %+v, want waiting", got)
	}
	if want := "w1 failed to upgrade, rolled back"; r.setting(keyRolloutStopped) != want {
		t.Fatalf("stopped = %q, want %q", r.setting(keyRolloutStopped), want)
	}
	f := r.fakes["w1"]
	if !sawCall(f, "Wait(helper-w1") || !sawCall(f, "StopRemove(helper-w1") {
		t.Fatalf("the helper's exit was not awaited and the helper removed: %v", callNames(f))
	}
	if len(r.fakes["w2"].Specs) != 0 {
		t.Fatal("the rollout went on after a failure")
	}
}

// A pull that fails changes nothing on the node and stops the rollout too.
func TestRolloutPullFailure(t *testing.T) {
	r := newRolloutRig(t)
	r.fakes["w1"].Err = map[string]error{"Pull": errors.New("registry down")}
	if err := r.ro.run(context.Background()); err == nil {
		t.Fatal("want the failure")
	}
	if got := r.state(t, "w1"); got.State != stateFailed || got.Error == "" {
		t.Fatalf("w1 = %+v", got)
	}
	if len(r.fakes["w1"].Specs) != 0 {
		t.Fatal("the helper ran without the image")
	}
	if r.setting(keyRolloutStopped) == "" {
		t.Fatal("rollout not stopped")
	}
}

// UpdateWorkers clears a stop, sets the target to the running version and
// queues the rollout; one already queued refuses a second.
func TestUpdateWorkers(t *testing.T) {
	n := newTwoNodes(t)
	n.orch.cfg.Version = "v9.9.9"
	ctx := context.Background()
	must(t, n.orch.store.Settings.Set(ctx, keyRolloutStopped, "w1 failed to upgrade, rolled back"))
	j, err := n.orch.UpdateWorkers(ctx, &Principal{})
	must(t, err)
	if j.Kind != string(kindWorkerRollout) {
		t.Fatalf("job kind = %q", j.Kind)
	}
	if v, _, _ := n.orch.store.Settings.Get(ctx, keyRolloutStopped); v != "" {
		t.Fatalf("stop not cleared: %q", v)
	}
	if v, _, _ := n.orch.store.Settings.Get(ctx, keyRolloutTarget); v != "v9.9.9" {
		t.Fatalf("target = %q", v)
	}
	if _, err := n.orch.UpdateWorkers(ctx, &Principal{}); err == nil {
		t.Fatal("a second rollout was queued over a pending one")
	}
}

// Boot queues the rollout only when the admin's target is this build and the
// rollout is not stopped.
func TestRolloutAfterBoot(t *testing.T) {
	for _, c := range []struct {
		name, target, stopped string
		want                  bool
	}{
		{"target is this build", "v9.9.9", "", true},
		{"target without the v", "9.9.9", "", true},
		{"another target", "v9.8.0", "", false},
		{"no target", "", "", false},
		{"stopped", "v9.9.9", "w1 failed to upgrade, rolled back", false},
	} {
		n := newTwoNodes(t)
		n.orch.cfg.Version = "v9.9.9"
		ctx := context.Background()
		if c.target != "" {
			must(t, n.orch.store.Settings.Set(ctx, keyRolloutTarget, c.target))
		}
		if c.stopped != "" {
			must(t, n.orch.store.Settings.Set(ctx, keyRolloutStopped, c.stopped))
		}
		n.orch.rolloutAfterBoot(ctx)
		if got := n.orch.rolloutActive(ctx); got != c.want {
			t.Errorf("%s: queued = %v, want %v", c.name, got, c.want)
		}
	}
}

// A node coming back online behind the target is picked up; one that is
// current is not.
func TestWorkerBack(t *testing.T) {
	n := newTwoNodes(t)
	n.orch.cfg.Version = "v9.9.9"
	ctx := context.Background()
	must(t, n.orch.store.Settings.Set(ctx, keyRolloutTarget, "v9.9.9"))
	w, err := n.st.Nodes.Get(ctx, "w1")
	must(t, err)

	if !n.orch.workerBehind(w, &worker.Facts{Version: "v9.8.0"}) || !n.orch.workerBehind(w, &worker.Facts{}) {
		t.Fatal("a different or empty version is behind")
	}
	if n.orch.workerBehind(w, &worker.Facts{Version: "9.9.9"}) || n.orch.workerBehind(w, nil) {
		t.Fatal("a current or unknown worker is not behind")
	}
	n.orch.workerBack(ctx, w, &worker.Facts{Version: "v9.9.9"})
	if n.orch.rolloutActive(ctx) {
		t.Fatal("a current worker queued a rollout")
	}
	n.orch.workerBack(ctx, w, &worker.Facts{Version: "v9.8.0"})
	if !n.orch.rolloutActive(ctx) {
		t.Fatal("a worker behind did not queue a rollout")
	}
}

// panel_prev_version follows panel_version across boots; booting the same
// build twice changes nothing.
func TestRecordPanelVersion(t *testing.T) {
	n := newTwoNodes(t)
	ctx := context.Background()
	get := func(k string) string { v, _, _ := n.orch.store.Settings.Get(ctx, k); return v }
	boot := func(v string) { n.orch.cfg.Version = v; must(t, n.orch.recordPanelVersion(ctx)) }

	boot("v0.5.0")
	if get(keyPanelVersion) != "v0.5.0" || get(keyPanelPrevVersion) != "" {
		t.Fatalf("first boot: %q / %q", get(keyPanelVersion), get(keyPanelPrevVersion))
	}
	boot("v0.5.0")
	if get(keyPanelPrevVersion) != "" {
		t.Fatalf("the same build moved prev: %q", get(keyPanelPrevVersion))
	}
	boot("v0.6.0")
	if get(keyPanelVersion) != "v0.6.0" || get(keyPanelPrevVersion) != "v0.5.0" {
		t.Fatalf("upgrade: %q / %q", get(keyPanelVersion), get(keyPanelPrevVersion))
	}
	boot("v0.6.0")
	if get(keyPanelPrevVersion) != "v0.5.0" {
		t.Fatalf("a restart lost prev: %q", get(keyPanelPrevVersion))
	}
}

// With STACKR_FAKE_HOST the rollout does nothing and says so.
func TestRolloutFakeHostIsANoOp(t *testing.T) {
	n := newTwoNodes(t)
	n.orch.cfg.FakeHost = "/tmp/fake"
	var log bytes.Buffer
	if err := n.orch.runWorkerRollout(context.Background(), &jobs.Run{Log: &log}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(log.String(), "fake workers are not upgraded") {
		t.Fatalf("log = %q", log.String())
	}
}

// The Update tab's view: every worker with its stored state and whether it
// is behind.
func TestWorkerRolloutView(t *testing.T) {
	r := newRolloutRig(t)
	ctx := context.Background()
	o := r.n.orch
	must(t, o.store.Settings.Set(ctx, keyRolloutTarget, "v9.9.9"))
	must(t, o.store.Settings.Set(ctx, keyRolloutStopped, "w1 failed to upgrade, rolled back"))
	o.setUpgrade(ctx, "w1", nodeUpgrade{Version: "v9.9.9", State: stateFailed, Error: "x", At: time.Now()})

	v, err := o.WorkerRollout(ctx)
	must(t, err)
	if v.Target != "v9.9.9" || v.Stopped == "" || len(v.Nodes) != 1 || v.Nodes[0].Name != "w1" || v.Nodes[0].State != stateFailed {
		t.Fatalf("view = %+v", v)
	}
}

// A worker done for an older build reads behind once the panel moves on.
func TestWorkerRolloutViewDoneThenBehind(t *testing.T) {
	r := newRolloutRig(t)
	ctx := context.Background()
	o := r.n.orch
	o.setUpgrade(ctx, "w1", nodeUpgrade{Version: "v9.8.0", State: stateDone, At: time.Now()})
	o.mesh.Report(ctx, "w1", worker.Facts{Version: "v9.8.0"})
	v, err := o.WorkerRollout(ctx)
	must(t, err)
	if g := v.Nodes[0]; g.State != "" || !g.Behind {
		t.Fatalf("row = %+v", g)
	}
	must(t, o.store.Nodes.Create(ctx, store.Node{
		ID: "off", Name: "off", Slug: "off", Role: node.Worker, Tags: "[]", RangeCIDR: "10.230.0.0/16",
		Status: node.StatusOffline, CreatedAt: time.Now(),
	}))
	o.setUpgrade(ctx, "off", nodeUpgrade{Version: "v9.8.0", State: stateDone, At: time.Now()})
	v, err = o.WorkerRollout(ctx)
	must(t, err)
	for _, g := range v.Nodes {
		if g.Name == "off" && (g.State != "" || !g.Behind) {
			t.Fatalf("offline row = %+v", g)
		}
	}
	o.mesh.Report(ctx, "w1", worker.Facts{Version: "v9.9.9"})
	o.setUpgrade(ctx, "w1", nodeUpgrade{Version: "v9.9.9", State: stateDone, At: time.Now()})
	v, _ = o.WorkerRollout(ctx)
	if g := v.Nodes[len(v.Nodes)-1]; g.Name != "w1" || g.State != stateDone || g.Behind {
		t.Fatalf("row = %+v", g)
	}
}

// A stop leaves the nodes after the failed one waiting, but no job runs, so
// the view is not running (the tab offers Retry); a queued rollout is.
func TestWorkerRolloutRunning(t *testing.T) {
	r := newRolloutRig(t)
	ctx := context.Background()
	o := r.n.orch
	must(t, o.store.Settings.Set(ctx, keyRolloutStopped, "w1 failed to upgrade, rolled back"))
	o.setUpgrade(ctx, "w1", nodeUpgrade{Version: "v9.9.9", State: stateWaiting, At: time.Now()})
	if v, _ := o.WorkerRollout(ctx); v.Running {
		t.Fatalf("stopped rollout reads running: %+v", v)
	}
	_, err := o.UpdateWorkers(ctx, nil)
	must(t, err)
	if v, _ := o.WorkerRollout(ctx); !v.Running {
		t.Fatalf("queued rollout reads idle: %+v", v)
	}
}
