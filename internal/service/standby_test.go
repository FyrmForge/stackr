package service

import (
	"context"
	"slices"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/opmember"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// A standby: the primary's side is set up over the primary's exec (role and
// slot, the password on stdin only), the standby's container is made on its
// node with the member label, the data volume and the bootstrap command,
// and the replication password is in no argv anywhere. R4 still holds.
func TestAddStandbyMakesTheContainer(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := newTwoNodes(t)
	m := pgOn(t, n)
	var err error
	m, err = n.orch.operated.SetMajor(ctx, m, 17)
	must(t, err)
	dt, err := n.st.Tiles.Get(ctx, n.devT.ID)
	must(t, err)
	dt.ImageRef = "postgres:17"
	must(t, n.st.Tiles.Update(ctx, dt))
	if _, err := n.orch.AddStandby(ctx, n.devT.ID, n.ctl.ID); !conflict(err) {
		t.Fatalf("standby on the primary's node: %v, want a conflict (R4)", err)
	}
	r, err := n.orch.AddStandby(ctx, n.devT.ID, "w1")
	must(t, err)
	if _, err := n.orch.AddStandby(ctx, n.devT.ID, "w1"); !conflict(err) {
		t.Errorf("second standby on w1: %v, want a conflict (R4)", err)
	}
	if r.Phase != opmember.Starting {
		t.Errorf("phase = %q", r.Phase)
	}
	if len(n.wFake.Specs) != 1 {
		t.Fatalf("w1 ran %d containers", len(n.wFake.Specs))
	}
	spec := n.wFake.Specs[0]
	if spec.Labels[LabelMember] != r.ID || spec.Restart != "no" || spec.Name != r.Container ||
		spec.Volumes[0] != r.Volume+":/var/lib/postgresql/data" || len(spec.ExtraHosts) != 1 ||
		spec.ExtraHosts[0] != operated.Alias(n.devT.Slug, m.ID)+":10.208.1.2" || !strings.Contains(strings.Join(spec.Cmd, " "), "pg_basebackup") {
		t.Errorf("standby spec = %+v", spec)
	}
	row, err := n.orch.operated.Get(ctx, m.ID)
	must(t, err)
	if row.ReplPassword == "" {
		t.Fatal("no replication password stored")
	}
	// the password reaches the container as a file copied in before it starts,
	// and is in no env docker inspect shows
	for _, e := range spec.Env {
		if strings.Contains(e, row.ReplPassword) || strings.HasPrefix(e, "POSTGRES_PASSWORD=") {
			t.Errorf("a secret is in the standby's env: %s", strings.SplitN(e, "=", 2)[0])
		}
	}
	if got := string(n.wFake.Copied[" /etc/stackr.pgpass"]); !strings.Contains(got, ":stackr_repl:"+row.ReplPassword) {
		t.Errorf("passfile copied in = %q (%v)", got, callNames(n.wFake))
	}
	if cs := callNames(n.wFake); slices.Index(cs, "CopyTo(, /etc/stackr.pgpass)") < slices.Index(cs, "Create("+r.Container+", postgres:17)") ||
		slices.Index(cs, "CopyTo(, /etc/stackr.pgpass)") > slices.Index(cs, "Start()") {
		t.Errorf("the passfile is not copied between create and start: %v", cs)
	}
	var stdin string
	for _, c := range append(n.ctlFake.Calls(), n.wFake.Calls()...) {
		if strings.Contains(strings.Join(c.Args, " "), row.ReplPassword) {
			t.Errorf("the replication password is on the argv of %s", c.Method)
		}
		stdin += c.Stdin
	}
	if !strings.Contains(stdin, "stackr_repl") || !strings.Contains(stdin, mflow.SlotName(r.ID)) {
		t.Errorf("the role and the slot were not made over stdin: %q", stdin)
	}
	n.orch.vipAsync.Wait()
}

// The read alias: from another node it is the primary's VIP under the read
// name, until a standby on the consumer's node is in the read pool; then it
// is that node's network. A standby that is stopped, fenced, rebuilding or
// not caught up is out of the pool.
func TestReadTarget(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := newTwoNodes(t)
	m := pgOn(t, n)
	r, err := n.orch.AddStandby(ctx, n.devT.ID, "w1")
	must(t, err)
	n.orch.vipAsync.Wait()
	edge, err := n.st.Environments.Get(ctx, n.edge.ID)
	must(t, err)
	dev, err := n.st.Environments.Get(ctx, n.dev.ID)
	must(t, err)
	alias := operated.Alias(n.devT.Slug, m.ID)
	primaryHost := mflow.ReadAlias(alias) + ":10.208.1.2"

	want := func(what, host, net string, e store.Environment) {
		t.Helper()
		h, nn, err := n.orch.readTarget(ctx, e, m.ID)
		if err != nil || h != host || nn != net {
			t.Errorf("%s: host %q network %q (%v), want %q %q", what, h, nn, err, host, net)
		}
	}
	want("standby still copying", primaryHost, "", edge)
	want("consumer on the primary's node", "", "", dev)

	r, err = n.orch.members.SetPhase(ctx, r, opmember.Streaming)
	must(t, err)
	want("streaming standby on the consumer's node", "", operated.Network(m.ID), edge)
	want("consumer on the primary's node, standby elsewhere", "", "", dev)

	fenced, err := n.orch.members.SetFenced(ctx, r, true)
	must(t, err)
	want("fenced", primaryHost, "", edge)
	_, err = n.orch.members.SetFenced(ctx, fenced, false)
	must(t, err)
	stopped, err := n.orch.members.SetState(ctx, r, opmember.Stopped)
	must(t, err)
	want("stopped", primaryHost, "", edge)
	_, err = n.orch.members.SetState(ctx, stopped, opmember.Running)
	must(t, err)
	want("running again", "", operated.Network(m.ID), edge)
	r, _ = n.orch.members.Get(ctx, r.ID)
	_, err = n.orch.members.SetPhase(ctx, r, opmember.Rebuild)
	must(t, err)
	want("needs rebuild", primaryHost, "", edge)
}

// The tick's look at a standby: streaming by its own report puts it in the
// read pool; a lost slot marks it needs rebuild, stops it, opens the alert
// and keeps the panel from starting it; rebuild makes it again on its node.
func TestObserveStandbys(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := newTwoNodes(t)
	m := pgOn(t, n)
	r, err := n.orch.AddStandby(ctx, n.devT.ID, "w1")
	must(t, err)
	n.orch.vipAsync.Wait()
	slot := mflow.SlotName(r.ID)
	// the standby's container as docker reports it, and what each side answers
	n.wFake.Containers = append(n.wFake.Containers, docker.Container{ID: "rep1", State: "running", Labels: map[string]string{LabelMember: r.ID}})
	status := "reserved"
	n.ctlFake.ExecFn = func(cmd []string) string {
		if strings.Contains(strings.Join(cmd, " "), "pg_replication_slots") {
			return slot + "|" + status + "\n"
		}
		return ""
	}
	n.wFake.ExecFn = func(cmd []string) string { return "1,streaming" }

	phase := func() string {
		got, err := n.orch.members.Get(ctx, r.ID)
		must(t, err)
		return got.Phase
	}
	n.orch.observeStandbys(ctx)
	if phase() != opmember.Starting {
		t.Fatalf("one streaming probe: phase = %q, want it still starting (two in a row join the pool)", phase())
	}
	n.orch.observeStandbys(ctx)
	if phase() != opmember.Streaming {
		t.Fatalf("phase = %q, want streaming", phase())
	}
	if vs, err := n.orch.Standbys(ctx, n.devT.ID); err != nil || len(vs) != 2 || vs[1].State != "streaming" || !vs[1].ReadPool {
		t.Errorf("standbys = %+v %v", vs, err)
	}

	n.wFake.ExecFn = func(cmd []string) string { return "1," } // the receiver is not streaming
	n.orch.observeStandbys(ctx)
	if phase() != opmember.Streaming {
		t.Fatalf("one failed probe: phase = %q, want it still streaming", phase())
	}
	n.orch.observeStandbys(ctx)
	if phase() != opmember.Starting {
		t.Errorf("a standby that does not stream is %q, want starting", phase())
	}

	status = "lost"
	n.orch.observeStandbys(ctx)
	if phase() != opmember.Rebuild {
		t.Fatalf("lost slot: phase %q, want rebuild", phase())
	}
	if !sawCall(n.wFake, "Stop(rep1)") {
		t.Errorf("the standby kept running: %v", callNames(n.wFake))
	}
	if !n.orch.hasProblem(ctx, "alert."+AlertPGRebuild+"."+m.ID) {
		t.Error("no problem for a standby that needs a rebuild")
	}
	n.wFake.Containers[len(n.wFake.Containers)-1].State = "exited"
	before := len(n.wFake.Specs)
	n.orch.startMembers(ctx)
	if sawCall(n.wFake, "Start(rep1)") || len(n.wFake.Specs) != before {
		t.Error("the panel started a standby that needs a rebuild")
	}

	status = "reserved"
	nr, err := n.orch.RebuildStandbyOf(ctx, n.devT.ID, r.ID)
	must(t, err)
	if nr.ID == r.ID || nr.Node != "w1" || nr.State != "starting" {
		t.Errorf("rebuilt standby = %+v", nr)
	}
	if _, err := n.orch.members.Get(ctx, r.ID); err == nil {
		t.Error("the old member row stayed")
	}
	if n.orch.hasProblem(ctx, "alert."+AlertPGRebuild+"."+m.ID) {
		t.Error("the rebuild alert stayed open")
	}
	if _, err := n.orch.RebuildStandbyOf(ctx, n.devT.ID, "nope"); err == nil {
		t.Error("rebuilt a member that is not there")
	}
	n.orch.vipAsync.Wait()
}
