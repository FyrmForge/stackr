package service

import (
	"context"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/paneldb"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// rollbackRig is a controller whose upgrade helper exited with code, its
// proxy on the target image (0.9.1) and the target tag in the registry.
type rollbackRig struct {
	n   *twoNodes
	reg *fakeRegistry
	job store.Job
}

func newRollbackRig(t *testing.T, code int) *rollbackRig {
	t.Helper()
	n := newTwoNodes(t)
	ctx := context.Background()
	oldImg, newImg := installspec.Image("v0.9.0"), installspec.Image("v0.9.1")
	n.ctlFake.Containers = []docker.Container{
		{ID: "up", Name: "stackr-upgrader", State: "exited", Labels: map[string]string{installspec.LabelRole: "upgrader"}},
		{ID: "pn", Name: "stackr-0.9.0", State: "running", Image: oldImg, Labels: map[string]string{installspec.LabelRole: "panel"}},
		{ID: "px", Name: installspec.ProxyName, State: "running", Image: newImg, Labels: map[string]string{installspec.LabelRole: "proxy", installspec.LabelSpec: newImg}},
	}
	n.ctlFake.Details = map[string]docker.Detail{"up": {State: "exited", ExitCode: code, Image: newImg}}
	n.orch.upgrade.Docker = n.orch.docker
	n.orch.upgrade.ProxySpec = func(image string) docker.ContainerSpec {
		return docker.ContainerSpec{Name: installspec.ProxyName, Image: image, Labels: map[string]string{installspec.LabelRole: "proxy", installspec.LabelSpec: image}}
	}
	reg := &fakeRegistry{tags: map[string]map[string]string{stackrRepo: {"0.9.0": "sha256:old", "0.9.1": "sha256:new"}}}
	srv := httptest.NewServer(reg)
	t.Cleanup(srv.Close)
	n.orch.regs.WithBase(srv.URL)
	stackrDigests.Store("0.9.1", "sha256:new")
	must(t, n.st.Settings.Set(ctx, keyRolloutTarget, "v0.9.1"))
	// the old panel ended the job when it launched the helper
	j := store.Job{ID: uuid.NewString(), Kind: string(kindUpgrade), State: "done", Payload: `{"tag":"v0.9.1"}`, LockSet: store.StringList{}, CreatedAt: time.Now()}
	must(t, n.st.Jobs.Create(ctx, j))
	return &rollbackRig{n: n, reg: reg, job: j}
}

// A helper that failed leaves the old panel booting over the target's side
// effects: the proxy goes back, the target tag leaves the registry, the
// rollout aimed at it is dropped and the job reads failed with the line.
func TestUndoFailedUpgrade(t *testing.T) {
	r := newRollbackRig(t, 1)
	ctx := context.Background()
	r.n.orch.undoFailedUpgrade(ctx)

	var ran []string
	for _, s := range r.n.ctlFake.Specs {
		ran = append(ran, s.Name+"="+s.Image)
	}
	if want := installspec.ProxyName + "=" + installspec.Image("v0.9.0"); !slices.Contains(ran, want) {
		t.Errorf("proxy not recreated on the previous image: %v", ran)
	}
	if r.reg.has(stackrRepo, "0.9.1") || !r.reg.has(stackrRepo, "0.9.0") {
		t.Errorf("registry tags after rollback: %v", r.reg.tags[stackrRepo])
	}
	if _, ok := stackrDigests.Load("0.9.1"); ok {
		t.Error("target digest still recorded")
	}
	if v, _, _ := r.n.st.Settings.Get(ctx, keyRolloutTarget); v != "" {
		t.Errorf("rollout target %q kept", v)
	}
	j, err := r.n.st.Jobs.Get(ctx, r.job.ID)
	must(t, err)
	if j.State != "failed" || j.Error != upgradeRolledBackMsg {
		t.Errorf("job = %s %q", j.State, j.Error)
	}
	if got := r.n.orch.LastUpgradeError(ctx); got != upgradeRolledBackMsg {
		t.Errorf("Update tab line = %q", got)
	}
	if !slices.ContainsFunc(r.n.ctlFake.Calls(), func(c dockerfake.Call) bool { return c.Method == "StopRemove" && c.Args[0] == "up" }) {
		t.Error("failed helper kept: a later boot would roll back again")
	}
}

// A helper that passed changes nothing.
func TestUndoFailedUpgradeLeavesASuccess(t *testing.T) {
	r := newRollbackRig(t, 0)
	ctx := context.Background()
	r.n.orch.cfg.Version = "0.9.1" // this panel is the target
	r.n.orch.undoFailedUpgrade(ctx)
	if len(r.n.ctlFake.Specs) != 0 || !r.reg.has(stackrRepo, "0.9.1") {
		t.Error("a passed upgrade was touched")
	}
	if j, _ := r.n.st.Jobs.Get(ctx, r.job.ID); j.State != "done" {
		t.Errorf("job = %s", j.State)
	}
	if r.n.orch.LastUpgradeError(ctx) != "" {
		t.Error("error line on a good upgrade")
	}
}

// A helper that rolled back cleanly exits 0 (so Docker does not rerun it);
// this panel is not the target, so the rollback still runs.
func TestUndoFailedUpgradeAfterCleanExit(t *testing.T) {
	r := newRollbackRig(t, 0)
	ctx := context.Background()
	r.n.orch.cfg.Version = "v0.9.0"
	r.n.orch.undoFailedUpgrade(ctx)
	if r.reg.has(stackrRepo, "0.9.1") {
		t.Error("target tag kept")
	}
	if j, _ := r.n.st.Jobs.Get(ctx, r.job.ID); j.State != "failed" || j.Error != upgradeRolledBackMsg {
		t.Errorf("job = %s %q", j.State, j.Error)
	}
	if !slices.ContainsFunc(r.n.ctlFake.Calls(), func(c dockerfake.Call) bool { return c.Method == "StopRemove" && c.Args[0] == "up" }) {
		t.Error("helper kept")
	}
}

// The installer writes no upgrade job. The target of the failed trial is the
// helper's image tag: an older job for the running version is left alone and
// the running version's image stays in the registry.
func TestUndoFailedUpgradeKeepsRunningVersion(t *testing.T) {
	r := newRollbackRig(t, 1)
	ctx := context.Background()
	r.n.orch.cfg.Version = "0.9.0"
	// a newer job naming the running version; the helper's target is 0.9.1
	must(t, r.n.st.Jobs.Create(ctx, store.Job{ID: uuid.NewString(), Kind: string(kindUpgrade), State: "done", Payload: `{"tag":"v0.9.0"}`, LockSet: store.StringList{}, CreatedAt: time.Now().Add(time.Hour)}))
	stackrDigests.Store("0.9.0", "sha256:old")
	r.n.orch.undoFailedUpgrade(ctx)
	if !r.reg.has(stackrRepo, "0.9.0") || r.reg.has(stackrRepo, "0.9.1") {
		t.Errorf("registry tags: %v", r.reg.tags[stackrRepo])
	}
	if _, ok := stackrDigests.Load("0.9.0"); !ok {
		t.Error("running version's digest dropped")
	}
	js, err := r.n.st.Jobs.ListByState(ctx, "failed")
	must(t, err)
	if len(js) != 1 || js[0].ID != r.job.ID {
		t.Errorf("failed jobs: %v", js)
	}
}

// A move helper that exited with the journal not showing the move fails the
// move job with the helper's log, is removed, and never touches the Update job.
func TestUndoFailedUpgradeSettlesAMoveHelper(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	newImg := installspec.Image("v0.9.1")
	n.ctlFake.Containers = []docker.Container{
		{ID: "mv", Name: "stackr-upgrader", State: "exited", Labels: map[string]string{installspec.LabelRole: "upgrader", "stackr.move": "m1"}},
	}
	n.ctlFake.Details = map[string]docker.Detail{"mv": {State: "exited", ExitCode: 1, Image: newImg}}
	n.orch.cfg.Version = "v0.9.0"
	mk := func(kind, payload string) store.Job {
		j := store.Job{ID: uuid.NewString(), Kind: kind, State: "done", Payload: payload, LockSet: store.StringList{}, CreatedAt: time.Now()}
		must(t, n.st.Jobs.Create(ctx, j))
		return j
	}
	mv := mk(kindMovePanelDB, `{"node_id":"c","move_id":"m1"}`)
	up := mk(string(kindUpgrade), `{"tag":"v0.9.1"}`)
	must(t, paneldb.WriteJournal(n.orch.cfg.DataDir, paneldb.Journal{Phase: paneldb.PhaseNone}))
	n.orch.undoFailedUpgrade(ctx)
	if j, _ := n.st.Jobs.Get(ctx, mv.ID); j.State != "failed" || !strings.Contains(j.Error, "did not go through") {
		t.Errorf("move job = %s %q", j.State, j.Error)
	}
	if j, _ := n.st.Jobs.Get(ctx, up.ID); j.State != "done" || j.Error != "" {
		t.Errorf("update job = %s %q", j.State, j.Error)
	}
	if !slices.ContainsFunc(n.ctlFake.Calls(), func(c dockerfake.Call) bool { return c.Method == "StopRemove" && c.Args[0] == "mv" }) {
		t.Error("helper kept")
	}
	if len(n.ctlFake.Specs) != 0 {
		t.Error("the upgrade rollback ran for a move helper")
	}
}

// A move helper that fails before the swap is settled when the hold ends, with
// no reboot: the move job fails with the log, the Update job is untouched.
func TestWatchUpgraderSettlesAFailedMoveHelper(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	watchFast(t, n)
	lbl := map[string]string{installspec.LabelRole: "upgrader", installspec.LabelMove: "m1"}
	n.ctlFake.Edit(func() {
		n.ctlFake.Containers = []docker.Container{{ID: "mv", Name: "stackr-upgrader", State: "running", Labels: lbl}}
		n.ctlFake.Details = map[string]docker.Detail{"mv": {State: "exited", ExitCode: 1}}
	})
	mk := func(kind, payload string) store.Job {
		j := store.Job{ID: uuid.NewString(), Kind: kind, State: "done", Payload: payload, LockSet: store.StringList{}, CreatedAt: time.Now()}
		must(t, n.st.Jobs.Create(ctx, j))
		return j
	}
	mv := mk(kindMovePanelDB, `{"node_id":"c","move_id":"m1"}`)
	up := mk(string(kindUpgrade), `{"tag":"v0.9.1"}`)
	must(t, paneldb.WriteJournal(n.orch.cfg.DataDir, paneldb.Journal{Phase: paneldb.PhaseNone}))
	n.orch.bootUpgradeHold(ctx)
	n.ctlFake.Edit(func() { n.ctlFake.Containers[0].State = "exited" })
	released(t, n)
	if j, _ := n.st.Jobs.Get(ctx, mv.ID); j.State != "failed" || !strings.Contains(j.Error, "did not go through") {
		t.Errorf("move job = %s %q", j.State, j.Error)
	}
	if j, _ := n.st.Jobs.Get(ctx, up.ID); j.State != "done" || j.Error != "" {
		t.Errorf("update job = %s %q", j.State, j.Error)
	}
}

func TestHelperCause(t *testing.T) {
	for name, c := range map[string]struct{ in, want string }{
		"json with err": {
			`{"level":"info","ts":0,"msg":"earlier"}` + "\n" +
				`{"level":"info","ts":1,"msg":"WARN panel database move: rolling back direction=in from_phase=gating err=\"new panel: the container restarted during the health check\""}` + "\n",
			"new panel: the container restarted during the health check",
		},
		"plain":  {"x\nstackrd upgrade-swap: boom\n", "stackrd upgrade-swap: boom"},
		"empty":  {"", ""},
		"blanks": {"\n  \n", ""},
	} {
		if got := helperCause(c.in); got != c.want {
			t.Errorf("%s: helperCause = %q, want %q", name, got, c.want)
		}
	}
}
