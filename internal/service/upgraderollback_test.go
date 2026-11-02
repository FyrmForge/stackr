package service

import (
	"context"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/installspec"
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
	n.ctlFake.Details = map[string]docker.Detail{"up": {State: "exited", ExitCode: code}}
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
