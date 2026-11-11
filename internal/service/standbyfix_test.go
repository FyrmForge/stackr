package service

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/promote"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/opmember"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// readerOn makes edgeT (on w1) a consumer of instance m that refs a *_read
// output, running there.
func readerOn(t *testing.T, n *twoNodes, m store.OperatedInstance) {
	t.Helper()
	ctx := context.Background()
	from := "shop:dev:" + n.devT.Slug
	sl, err := n.orch.tiles.Create(ctx, store.Tile{StackID: n.stack, EnvironmentID: n.edge.ID, Name: "db", Kind: tile.Slice, ProvisionFrom: &from})
	must(t, err)
	p := store.Provision{ID: uuid.NewString(), TileID: sl.ID, InstanceID: m.ID, DBName: "a", DBUser: "a", DBPassword: "x", CreatedAt: time.Now()}
	must(t, n.st.Provisions.Create(ctx, p))
	must(t, n.st.Bindings.Create(ctx, store.Binding{ID: uuid.NewString(), ProvisionID: p.ID, ConsumerTileID: n.edgeT.ID,
		Access: tile.Write, DBUser: "b", DBPassword: "y", Outputs: "{}", CreatedAt: time.Now()}))
	et, err := n.st.Tiles.Get(ctx, n.edgeT.ID)
	must(t, err)
	et.EnvJSON = `{"URL":"${{ tile.db.url_read }}"}`
	must(t, n.st.Tiles.Update(ctx, et))
	n.wFake.Containers = append(n.wFake.Containers, docker.Container{ID: "edge1", State: "running",
		Labels: map[string]string{tile.LabelTile: n.edgeT.ID, tile.LabelRole: "replica"}})
}

func deployed(t *testing.T, n *twoNodes) bool {
	t.Helper()
	_, ok, err := n.orch.jobRows.Last(context.Background(), n.edgeT.ID)
	must(t, err)
	return ok
}

// A routing change owed to consumers is recorded on the member and retried by
// the tick until every affected consumer was redeployed; only consumers that
// ref a *_read output are redeployed.
func TestRoutePendingRetries(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := newTwoNodes(t)
	m := pgOn(t, n)
	r, err := n.orch.AddStandby(ctx, adminP(), n.devT.ID, "w1")
	must(t, err)
	n.orch.vipAsync.Wait()
	readerOn(t, n, m)

	r, err = n.orch.members.SetPhase(ctx, r, opmember.Streaming)
	must(t, err)
	n.wFake.Err = map[string]error{"List": errors.New("node busy")}
	n.orch.routePending(ctx)
	if got, _ := n.orch.members.Get(ctx, r.ID); got.Routed || deployed(t, n) {
		t.Fatalf("routed %v deployed %v after a failed redeploy: it must stay owed", got.Routed, deployed(t, n))
	}
	delete(n.wFake.Err, "List")
	n.orch.routePending(ctx)
	if got, _ := n.orch.members.Get(ctx, r.ID); !got.Routed || !deployed(t, n) {
		t.Fatalf("routed %v deployed %v: the tick did not finish the redeploy", got.Routed, deployed(t, n))
	}

	// a standby that leaves the pool is owed the same
	r, _ = n.orch.members.Get(ctx, r.ID)
	_, err = n.orch.members.SetPhase(ctx, r, opmember.Starting)
	must(t, err)
	if got, _ := n.orch.members.Get(ctx, r.ID); got.Routed == opmember.InReadPool(got) {
		t.Fatal("leaving the pool is not owed")
	}
	n.orch.routePending(ctx)
	if got, _ := n.orch.members.Get(ctx, r.ID); got.Routed {
		t.Error("the change was not routed")
	}

	other := Tile{EnvJSON: `{"URL":"${{ tile.db.url }}"}`}
	if usesRead(other) || !usesRead(Tile{Command: "x ${{ tile.db.host_read }}"}) {
		t.Error("usesRead misjudges which consumers ref a read output")
	}
	n.orch.vipAsync.Wait()
}

// Fencing a standby stops it and takes it out of the pool at once; clearing
// the fence does not put it back: it needs to be seen streaming again.
func TestFenceStandby(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := newTwoNodes(t)
	pgOn(t, n)
	r, err := n.orch.AddStandby(ctx, adminP(), n.devT.ID, "w1")
	must(t, err)
	n.orch.vipAsync.Wait()
	n.wFake.Containers = append(n.wFake.Containers, docker.Container{ID: "rep1", State: "running", Labels: map[string]string{LabelMember: r.ID}})
	r, err = n.orch.members.SetPhase(ctx, r, opmember.Streaming)
	must(t, err)

	must(t, n.orch.FenceMember(ctx, r.ID, true))
	if !sawCall(n.wFake, "Stop(rep1)") {
		t.Errorf("a fenced standby kept running: %v", callNames(n.wFake))
	}
	got, _ := n.orch.members.Get(ctx, r.ID)
	if opmember.InReadPool(got) || got.Phase != opmember.Starting {
		t.Errorf("fenced: phase %q, in pool %v", got.Phase, opmember.InReadPool(got))
	}
	must(t, n.orch.FenceMember(ctx, r.ID, false))
	if got, _ = n.orch.members.Get(ctx, r.ID); opmember.InReadPool(got) || got.Phase != opmember.Starting {
		t.Errorf("unfenced: phase %q, in pool %v; it is back only once streaming", got.Phase, opmember.InReadPool(got))
	}
}

// A standby that stops (or is stopped, fenced) leaves the pool at once, not
// after two probes.
func TestStandbyLeavesAtOnceWhenDown(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := newTwoNodes(t)
	pgOn(t, n)
	r, err := n.orch.AddStandby(ctx, adminP(), n.devT.ID, "w1")
	must(t, err)
	n.orch.vipAsync.Wait()
	slot := mflow.SlotName(r.ID)
	n.ctlFake.ExecFn = func(cmd []string) string {
		if strings.Contains(strings.Join(cmd, " "), "pg_replication_slots") {
			return slot + "|reserved\n"
		}
		return ""
	}
	n.wFake.Containers = append(n.wFake.Containers, docker.Container{ID: "rep1", State: "running", Labels: map[string]string{LabelMember: r.ID}})
	n.wFake.ExecFn = func([]string) string { return "1,streaming" }
	n.orch.observeStandbys(ctx)
	n.orch.observeStandbys(ctx)
	if got, _ := n.orch.members.Get(ctx, r.ID); got.Phase != opmember.Streaming {
		t.Fatalf("phase %q", got.Phase)
	}
	n.wFake.Containers[len(n.wFake.Containers)-1].State = "exited"
	n.orch.observeStandbys(ctx)
	if got, _ := n.orch.members.Get(ctx, r.ID); got.Phase != opmember.Starting {
		t.Errorf("a stopped standby is still %q", got.Phase)
	}
}

// A standby removed while its primary is not running keeps its row, as
// 'removing', until the slot is really dropped; the tick finishes it. A slot
// no row stands for is dropped by the tick.
func TestRemoveStandbyKeepsRowUntilSlotDropped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := newTwoNodes(t)
	m := pgOn(t, n)
	r, err := n.orch.AddStandby(ctx, adminP(), n.devT.ID, "w1")
	must(t, err)
	n.orch.vipAsync.Wait()
	for i := range n.ctlFake.Containers {
		if n.ctlFake.Containers[i].ID == "pg" {
			n.ctlFake.Containers[i].State = "exited"
		}
	}
	if err := n.orch.RemoveStandby(ctx, r.ID); !errors.Is(err, errPrimaryDown) {
		t.Fatalf("remove with the primary down: %v", err)
	}
	if got, err := n.orch.members.Get(ctx, r.ID); err != nil || got.State != opmember.Removing {
		t.Fatalf("the row went with the slot undropped: %+v %v", got, err)
	}
	n.orch.finishRemovals(ctx)
	if _, err := n.orch.members.Get(ctx, r.ID); err != nil {
		t.Fatal("finished while the primary is still down")
	}
	for i := range n.ctlFake.Containers {
		n.ctlFake.Containers[i].State = "running"
	}
	n.orch.finishRemovals(ctx)
	if _, err := n.orch.members.Get(ctx, r.ID); err == nil {
		t.Error("the removal was not finished once the primary runs")
	}
	var stdin string
	for _, c := range n.ctlFake.Calls() {
		stdin += c.Stdin
	}
	if !strings.Contains(stdin, "pg_drop_replication_slot") || !strings.Contains(stdin, "NOLOGIN") {
		t.Errorf("slot not dropped or the role not closed with the last standby: %q", stdin)
	}

	// a stray slot: no row stands for it
	n.ctlFake.ExecFn = func(cmd []string) string {
		if strings.Contains(strings.Join(cmd, " "), "pg_replication_slots") {
			return "stackr_stray|reserved\n"
		}
		return ""
	}
	tl, _ := n.orch.tiles.Get(ctx, n.devT.ID)
	before := len(n.ctlFake.Calls())
	n.orch.reconcileSlots(ctx, tl, m)
	var dropped bool
	for _, c := range n.ctlFake.Calls()[before:] {
		dropped = dropped || strings.Contains(c.Stdin, "stackr_stray") && strings.Contains(c.Stdin, "pg_drop_replication_slot")
	}
	if !dropped {
		t.Error("a slot without a member row was kept")
	}
	n.orch.vipAsync.Wait()
}

// The replication lines are those of the standbys that exist: the new one's
// network on add, none (and a closed role) once the last is gone.
func TestReplicationLinesFollowStandbys(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := newTwoNodes(t)
	pgOn(t, n)
	r, err := n.orch.AddStandby(ctx, adminP(), n.devT.ID, "w1")
	must(t, err)
	n.orch.vipAsync.Wait()
	var argv string
	for _, c := range n.ctlFake.Calls() {
		argv += strings.Join(c.Args, " ") + "\n"
	}
	if !strings.Contains(argv, "host replication stackr_repl "+r.Subnet+" scram-sha-256") || strings.Contains(argv, "10.208.0.0/12") {
		t.Errorf("hba lines on add: %s", argv)
	}
	if r.Subnet == "" {
		t.Fatal("no subnet on the standby")
	}
	must(t, n.orch.RemoveStandby(ctx, r.ID))
	var stdin string
	for _, c := range n.ctlFake.Calls() {
		stdin += c.Stdin
	}
	if !strings.Contains(stdin, "ALTER ROLE stackr_repl NOLOGIN") {
		t.Error("the replication role stayed open with no standby")
	}
	n.orch.vipAsync.Wait()
}

// A standby's recovery limits may not be below the primary's: raising a
// limit lets the standbys restart first, lowering it waits for the primary.
func TestStandbyMayRestart(t *testing.T) {
	live := func(mc string) map[string]mflow.LiveSetting {
		l := map[string]mflow.LiveSetting{}
		for n, d := range recoveryLimits {
			l[n] = mflow.LiveSetting{Value: strconv.Itoa(d)}
		}
		l["max_connections"] = mflow.LiveSetting{Value: mc}
		return l
	}
	for _, c := range []struct {
		name string
		cur  map[string]string
		live string
		want bool
	}{
		{"raised, primary still on the old", map[string]string{"max_connections": "200"}, "100", true},
		{"lowered, primary not restarted yet", map[string]string{"max_connections": "50"}, "100", false},
		{"lowered, primary restarted", map[string]string{"max_connections": "50"}, "50", true},
		{"default after an unset, primary higher", nil, "200", false},
		{"unrelated change", map[string]string{"work_mem": "8MB"}, "100", true},
	} {
		if got := standbyMayRestart(c.cur, live(c.live)); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	if standbyMayRestart(nil, nil) {
		t.Error("restarted with the primary's limits unknown")
	}
}

// A settings change reaches every running standby (their conf file is written
// over exec), and one that refuses puts back the ones before it.
func TestSettingsReachStandbys(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := newTwoNodes(t)
	m := pgOn(t, n)
	r, err := n.orch.AddStandby(ctx, adminP(), n.devT.ID, "w1")
	must(t, err)
	n.orch.vipAsync.Wait()
	n.wFake.Containers = append(n.wFake.Containers, docker.Container{ID: "rep1", State: "running", Labels: map[string]string{LabelMember: r.ID}})
	n.wFake.ExecFn = func(cmd []string) string {
		if strings.Contains(strings.Join(cmd, " "), "pg_settings") {
			return "work_mem|user\n"
		}
		return ""
	}
	tl, _ := n.orch.tiles.Get(ctx, n.devT.ID)
	before := len(n.wFake.Calls())
	sbs, err := n.orch.applyToStandbys(ctx, tl, m.ID, map[string]string{"work_mem": "8MB"}, []string{"work_mem"}, nil)
	if err != nil || len(sbs) != 1 {
		t.Fatalf("applied to %d standbys: %v", len(sbs), err)
	}
	var wrote bool
	for _, c := range n.wFake.Calls()[before:] {
		wrote = wrote || c.Method == "Exec" && strings.Contains(strings.Join(c.Args, " "), "stackr.conf")
	}
	if !wrote {
		t.Errorf("no conf written on the standby: %v", callNames(n.wFake)[before:])
	}
	// a stopped standby is not asked; it takes the settings at its next start
	n.wFake.Containers[len(n.wFake.Containers)-1].State = "exited"
	if sbs, err := n.orch.applyToStandbys(ctx, tl, m.ID, map[string]string{"work_mem": "9MB"}, []string{"work_mem"}, nil); err != nil || len(sbs) != 0 {
		t.Errorf("stopped standby: %d, %v", len(sbs), err)
	}
	n.orch.vipAsync.Wait()
}

// One lock per instance for adding, removing, rebuilding and fencing: a
// second change waits for the first, and the stack file's list goes through it
// without deadlocking.
func TestTopologyLock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	n := newTwoNodes(t)
	m := pgOn(t, n)
	var done atomic.Bool
	mu := n.orch.topoLock(m.ID)
	mu.Lock()
	go func() {
		_, _ = n.orch.AddStandby(ctx, adminP(), n.devT.ID, "w1")
		done.Store(true)
	}()
	time.Sleep(100 * time.Millisecond)
	if done.Load() {
		t.Fatal("an add ran while the topology lock was held")
	}
	mu.Unlock()
	for i := 0; i < 100 && !done.Load(); i++ {
		time.Sleep(50 * time.Millisecond)
	}
	if !done.Load() {
		t.Fatal("the add never ran")
	}
	tl, _ := n.orch.tiles.Get(ctx, n.devT.ID)
	pctx := context.WithValue(withMayPlace(ctx, true), placeAdminKey{}, true)
	d, err := n.orch.standbyPlan(pctx, tl, nil)
	must(t, err)
	must(t, n.orch.setStandbys(ctx, tl, d))
	d, err = n.orch.standbyPlan(pctx, tl, []promote.Standby{{Node: "w1"}})
	must(t, err)
	must(t, n.orch.setStandbys(ctx, tl, d))
	n.orch.vipAsync.Wait()
}
