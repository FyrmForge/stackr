package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/opmember"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/volume"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/internal/vip"
	"github.com/FyrmForge/stackr/internal/worker"
	"github.com/google/uuid"
)

var passwordRE = regexp.MustCompile(`PASSWORD '[^']*'`)

func init() { // the polls are for a daemon; the fakes answer at once
	failPoll, failWait = time.Millisecond, 200*time.Millisecond
}

// sim is a node's docker that remembers: containers made, started and
// stopped are what List says next, and every change and exec lands in the
// rig's one ordered log.
type sim struct {
	Docker
	rig  *failRig
	node string
	mu   sync.Mutex
	cs   []simCtr
	n    int
}

type simCtr struct {
	docker.Container
	cmd []string
}

func (s *sim) List(_ context.Context, labels map[string]string) ([]docker.Container, error) {
	s.rig.mu.Lock()
	err := s.rig.listErr
	if s.rig.downNode == s.node {
		err = errors.New("no answer")
	}
	s.rig.mu.Unlock()
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []docker.Container
	for _, c := range s.cs {
		ok := true
		for k, v := range labels {
			ok = ok && c.Labels[k] == v
		}
		if ok {
			out = append(out, c.Container)
		}
	}
	return out, nil
}

func (s *sim) Create(_ context.Context, spec docker.ContainerSpec) (string, error) {
	s.rig.log(s.node, "create "+spec.Name, strings.Join(spec.Cmd, " "))
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	c := docker.Container{ID: fmt.Sprintf("%s-%d", s.node, s.n), Name: spec.Name, State: "created", Labels: spec.Labels, Nets: map[string]string{}}
	for _, na := range spec.Networks {
		ip := na.IPv4
		if ip == "" {
			ip = fmt.Sprintf("10.208.9.%d", s.n)
		}
		c.Nets[na.Name] = ip
		c.IPs = append(c.IPs, ip)
	}
	s.cs = append(s.cs, simCtr{c, spec.Cmd})
	s.rig.specs = append(s.rig.specs, spec)
	return c.ID, nil
}

func (s *sim) set(id, state string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.cs {
		if s.cs[i].ID == id {
			s.cs[i].State = state
			return s.cs[i].Name
		}
	}
	return id
}

// Run is create and start, as docker run is; its container answers its health gate.
func (s *sim) Run(ctx context.Context, spec docker.ContainerSpec) (string, error) {
	id, err := s.Create(ctx, spec)
	if err != nil {
		return "", err
	}
	return id, s.Start(ctx, id)
}

func (s *sim) Inspect(_ context.Context, id string) (docker.Detail, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.cs {
		if c.ID == id {
			return docker.Detail{Running: c.State == "running", Health: "healthy"}, nil
		}
	}
	return docker.Detail{}, nil
}

func (s *sim) Start(_ context.Context, id string) error {
	s.rig.log(s.node, "start "+s.set(id, "running"), "")
	return nil
}

func (s *sim) Stop(_ context.Context, id string) error {
	s.rig.mu.Lock()
	err := s.rig.stopErr
	s.rig.mu.Unlock()
	if err != nil {
		return err
	}
	s.rig.log(s.node, "stop "+s.set(id, "exited"), "")
	return nil
}

func (s *sim) StopRemove(_ context.Context, id string) error {
	s.mu.Lock()
	name := id
	s.cs = slices.DeleteFunc(s.cs, func(c simCtr) bool {
		if c.ID == id {
			name = c.Name
		}
		return c.ID == id
	})
	s.mu.Unlock()
	s.rig.log(s.node, "remove "+name, "")
	return nil
}

func (s *sim) Wait(context.Context, string) (int, error) { return 0, nil }

func (s *sim) Logs(_ context.Context, id string, _ int) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, c := range s.cs {
		if c.ID == id && len(c.cmd) > 0 && c.cmd[0] == "pg_controldata" {
			return s.rig.control, nil
		}
	}
	return "", nil
}

func (s *sim) Exec(_ context.Context, id string, cmd []string) (string, error) {
	c := strings.Join(cmd, " ")
	s.rig.log(s.node, "exec", c)
	if strings.Contains(c, "kill -INT 1") {
		s.set(id, "exited")
		return "", nil
	}
	return s.rig.answer(s.node, c), nil
}

func (s *sim) ExecStream(_ context.Context, _ string, cmd []string, stdin io.Reader) (io.Reader, func() error, error) {
	in := ""
	if stdin != nil {
		b, _ := io.ReadAll(stdin)
		in = string(b)
	}
	c := strings.Join(cmd, " ")
	s.rig.log(s.node, "exec", c+" "+in)
	return strings.NewReader(s.rig.answer(s.node, c+" "+in)), func() error { return nil }, nil
}

func (s *sim) MemberAddr(context.Context, string, string) (string, string, error) {
	return "", "10.209.4.0/24", nil
}

// failRig is a postgres instance on the controller with a streaming standby
// on w1, over two sims and one event log.
type failRig struct {
	n      *twoNodes
	t      *testing.T
	mu     sync.Mutex
	events []string
	specs  []docker.ContainerSpec
	// what the databases answer
	promoted map[string]bool
	lsn      map[string]string
	control  string
	inst     store.OperatedInstance
	sims     map[string]*sim
	// before is told the first time anything is changed
	before func()
	fired  bool
	// the leases the workers took; leaseErr makes a push fail
	pushed   map[string]worker.Lease
	leaseErr error
	listErr  error  // makes every docker list fail
	downNode string // the sim whose docker answers nothing (a node not yet marked offline)
	stopErr  error  // makes a docker stop fail
	// the primary's sync conf as last written, and the setting a reload made live
	syncFile, syncLive string
	reloadBroken       bool // a reload does not take
	// notArchived makes pg_stat_archiver say nothing was archived
	notArchived bool
}

// ageLease makes every recorded lease binding as old as ago.
func (r *failRig) ageLease(ago time.Duration) {
	r.t.Helper()
	ctx := context.Background()
	recs, err := r.n.orch.leaseRecs(ctx)
	must(r.t, err)
	for k, rec := range recs {
		rec.Ack = time.Now().Add(-ago)
		b, _ := json.Marshal(rec)
		must(r.t, r.n.orch.store.Settings.Set(ctx, leasePrefix+k, string(b)))
	}
}

func (r *failRig) log(node, what, detail string) {
	r.mu.Lock()
	if !r.fired && r.before != nil && what != "" && !strings.HasPrefix(what, "exec") {
		r.fired = true
		r.mu.Unlock()
		r.before()
		r.mu.Lock()
	}
	r.events = append(r.events, node+" "+what+" "+passwordRE.ReplaceAllString(detail, "PASSWORD '***'"))
	r.mu.Unlock()
}

func (r *failRig) answer(node, c string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case strings.Contains(c, ".get_auth('"): // the pooler's auth proof: the probe is found, the admin and the pooler role are not
		return "1,0,0"
	case strings.Contains(c, "stackr-sync.conf") && strings.Contains(c, "$1.new"):
		r.syncFile = ""
		if _, v, ok := strings.Cut(c, "synchronous_standby_names = '"); ok {
			r.syncFile, _, _ = strings.Cut(v, "'\n")
		}
	case strings.Contains(c, "pg_reload_conf"):
		if !r.reloadBroken {
			r.syncLive = r.syncFile
		}
	case strings.Contains(c, "current_setting('synchronous_standby_names')"):
		return r.syncLive
	case strings.Contains(c, "pg_is_in_recovery"):
		if r.promoted[node] {
			return "0"
		}
		return "1"
	case strings.Contains(c, "pg_promote"):
		r.promoted[node] = true
		return "1"
	case strings.Contains(c, "pg_last_wal_replay_lsn"):
		return r.lsn[node]
	case strings.Contains(c, "last_archived_wal"):
		if r.notArchived {
			return "0"
		}
		return "1"
	case strings.Contains(c, "pg_walfile_name"):
		return "000000010000000000000003"
	case strings.Contains(c, "pg_switch_wal"):
		return "0/4000000"
	}
	return ""
}

// order asserts that the log has an entry containing each of subs, in order.
func (r *failRig) order(subs ...string) {
	r.t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	at := 0
	for _, s := range subs {
		i := slices.IndexFunc(r.events[at:], func(e string) bool { return strings.Contains(e, s) })
		if i < 0 {
			r.t.Fatalf("no %q after event %d; log:\n%s", s, at, strings.Join(r.events, "\n"))
		}
		at += i + 1
	}
}

func (r *failRig) saw(sub string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.ContainsFunc(r.events, func(e string) bool { return strings.Contains(e, sub) })
}

func (r *failRig) members() (primary, replica InstanceMember) {
	r.t.Helper()
	ms, err := r.n.orch.members.List(context.Background(), r.inst.ID)
	must(r.t, err)
	for _, m := range ms {
		if m.Role == opmember.Primary {
			primary = m
		} else {
			replica = m
		}
	}
	return
}

// newFailRig is the instance of tile primary on its node, a standby on
// standbyNode, both running and streaming.
func newFailRig(t *testing.T, onWorker bool) *failRig {
	t.Helper()
	ctx := context.Background()
	n := newTwoNodes(t)
	r := &failRig{n: n, t: t, promoted: map[string]bool{}, lsn: map[string]string{}, sims: map[string]*sim{}}
	pt, standbyOn, primaryOn := n.devT, "w1", n.ctl.ID
	if onWorker {
		pt, standbyOn, primaryOn = n.edgeT, n.ctl.ID, "w1"
	}
	pt.Kind, pt.ImageRef = tile.Operated, "postgres:17"
	must(t, n.st.Tiles.Update(ctx, pt))
	m := pgOn(t, n)
	if onWorker { // pgOn made the instance on the controller's tile: move it to the edge tile
		must(t, n.st.OperatedInstances.Delete(ctx, m.ID))
		var err error
		m, err = n.orch.operated.Create(ctx, pt.ID, "postgres", "stackr", "")
		must(t, err)
		_, err = n.orch.members.CreatePrimary(ctx, m.ID)
		must(t, err)
	}
	m, err := n.orch.operated.SetMajor(ctx, m, 17)
	must(t, err)
	r.inst = m
	r.control = "Database cluster state:               shut down\nLatest checkpoint location:           0/5000028\n"
	r.promoted[primaryOn] = true
	r.lsn[standbyOn] = "0/5000100"
	// every Docker call goes through a sim; the controller's has the primary
	for id, base := range map[string]Docker{n.ctl.ID: n.ctlFake, "w1": n.wFake} {
		s := &sim{Docker: base, rig: r, node: id}
		if id == n.ctl.ID {
			s.node = "ctl"
		}
		if id == "w1" {
			s.Docker = &subnetFake{Fake: n.wFake, nets: map[string]bool{}}
		}
		r.sims[s.node] = s
		n.orch.docker.(*DockerRouter).Set(id, s)
	}
	pause := docker.Container{ID: "pause", State: "running", IPs: []string{"10.208.1.2"}, Labels: map[string]string{tile.LabelTile: pt.ID, tile.LabelRole: "pause"}}
	prim := docker.Container{ID: "pg", Name: "pg", State: "running", Labels: map[string]string{tile.LabelTile: pt.ID, tile.LabelRole: "replica"}}
	primarySim := r.sims["ctl"]
	if onWorker {
		primarySim = r.sims["w1"]
	}
	primarySim.cs = append(primarySim.cs, simCtr{Container: pause}, simCtr{Container: prim})
	// the data volume the tile's deploy would have declared
	_, _, err = n.orch.volumes.Declare(ctx, volume.Scope{Kind: "env", ID: pt.EnvironmentID}, pt.Slug+"-data", &m.ID)
	must(t, err)
	env := n.dev
	if onWorker {
		env = n.edge
	}
	must(t, n.st.Environments.SetSubnet(ctx, env.ID, "10.208.0.0/24"))
	if onWorker {
		must(t, n.st.TileVIPs.Set(ctx, pt.ID, "w1", "10.209.1.2"))
	}
	r.pushed = map[string]worker.Lease{}
	n.orch.leasePush = func(_ context.Context, id string, l worker.Lease) error {
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.leaseErr != nil {
			return r.leaseErr
		}
		r.pushed[id] = l
		return nil
	}
	rep, err := n.orch.AddStandby(ctx, pt.ID, standbyOn)
	must(t, err)
	n.orch.vipAsync.Wait()
	_, err = n.orch.members.SetPhase(ctx, rep, opmember.Streaming)
	must(t, err)
	r.mu.Lock()
	r.events = nil
	r.specs = nil
	r.mu.Unlock()
	t.Cleanup(n.orch.vipAsync.Wait)
	return r
}

func (r *failRig) tile() Tile {
	t, err := r.n.orch.tiles.Get(context.Background(), r.inst.TileID)
	must(r.t, err)
	return t
}

// A switchover goes in the order that loses nothing: the archive is drained
// (the segment in progress switched out and waited for), the primary is shut
// down cleanly (SIGINT, a fast shutdown) and stopped, the stopped cluster's
// control file proves a clean shutdown and its checkpoint is replayed by the
// standby, only then is the standby promoted; its container is made again as
// the primary (not the standby's script), the rows swap, the other side's slot
// is made on the new primary, the consumers are pointed at it and the old
// primary rejoins as a standby.
func TestSwitchoverHappyPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, false)
	o := r.n.orch
	v, err := o.Switchover(ctx, r.inst.TileID, "w1")
	must(t, err)
	if v.Role != opmember.Primary || v.Node != "w1" {
		t.Errorf("view = %+v, want the primary on w1", v)
	}
	r.order(
		"ctl exec sh -c kill -INT 1", // the clean stop
		"ctl stop pg",
		"ctl create stackr-ctl",                    // the control file of the stopped cluster
		"SELECT coalesce(pg_last_wal_replay_lsn()", // the standby has the shutdown checkpoint
		"SELECT pg_promote",
		"w1 create stackr-member", // recreated as the primary
		"w1 start stackr-member",
		"pg_create_physical_replication_slot", // the old primary's slot, on the new primary
		"ctl remove pg",                       // the old container goes
		"ctl create stackr-rewind",
		"ctl create stackr-member", // the old primary as a standby
	)

	p, rep := r.members()
	if p.NodeID != "w1" || p.Fenced || p.Container == "" {
		t.Errorf("primary row = %+v, want the member on w1", p)
	}
	if rep.NodeID != r.n.ctl.ID || rep.Role != opmember.Replica || rep.Fenced || rep.Phase != opmember.Starting {
		t.Errorf("old primary row = %+v, want an unfenced standby on the controller", rep)
	}
	if rep.Volume == "" {
		t.Error("the old primary's data volume is not on its row")
	}
	if _, ok := o.failoverOf(ctx, r.inst.ID); ok {
		t.Error("the cursor outlived the failover")
	}
	// the new primary is where the instance's containers are looked for
	if n, ok := o.primaryReroute(ctx, r.tile()); !ok || n != "w1" {
		t.Errorf("reroute = %q %v", n, ok)
	}
	// the pooler stays on the env's node: its verbs do not follow the primary
	r.sims["ctl"].cs = append(r.sims["ctl"].cs, simCtr{Container: docker.Container{ID: "pool", Name: "pool", State: "running", Labels: map[string]string{tile.LabelTile: r.tile().ID, tile.LabelRole: "pooler"}}})
	if _, err := o.tiles.Exec(ctx, r.tile().ID, "pool", []string{"true"}); err != nil {
		t.Errorf("exec in the pooler after the failover: %v", err)
	}
	// and the container it runs is the primary's: its command is not the standby's
	var promoted docker.ContainerSpec
	for _, s := range r.specs {
		if s.Labels[tile.LabelTile] != "" && s.Labels[LabelMember] == p.ID {
			promoted = s
		}
	}
	if promoted.Name == "" {
		t.Fatalf("no primary container was made; specs: %d", len(r.specs))
	}
	if strings.Contains(strings.Join(promoted.Cmd, " "), "pg_basebackup") {
		t.Error("the promoted primary would run the standby's copy script")
	}
	if got := promoted.Networks[0].Aliases; !slices.Contains(got, operated.Alias(r.tile().Slug, r.inst.ID)) {
		t.Errorf("aliases = %v, want the write alias", got)
	}
	if promoted.Networks[0].IPv4 == "" {
		t.Error("the primary's address is not pinned")
	}
	// the standby that is left follows the new primary by its address
	if h, err := o.memberHost(ctx, rep, rep.NodeID); err != nil || !strings.HasSuffix(h, ":"+promoted.Networks[0].IPv4) {
		t.Errorf("old primary's host line = %q %v, want the new primary's address", h, err)
	}
}

// Every step can be cut off by a dying panel and picked up again by the
// tick: it ends with the same rows and no cursor.
func TestFailoverResumesAfterCrashAtEveryStep(t *testing.T) {
	t.Parallel()
	for _, step := range failoverSteps[foSwitchover] {
		t.Run(step, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			r := newFailRig(t, false)
			o := r.n.orch
			o.failCrashAfter = step
			if _, err := o.Switchover(ctx, r.inst.TileID, "w1"); err == nil {
				t.Fatal("the crash did not stop the run")
			}
			if f, ok := o.failoverOf(ctx, r.inst.ID); !ok {
				t.Fatal("no cursor after the crash")
			} else if steps := failoverSteps[foSwitchover]; slices.Index(steps, f.Step) < slices.Index(steps, step) {
				t.Fatalf("cursor at %q, behind the crashed step %q", f.Step, step)
			}
			// while the cursor stands, the tick leaves the instance alone
			if !o.failoverActive(ctx, r.inst.ID) {
				t.Fatal("not active")
			}
			o.failCrashAfter = ""
			o.resumeFailovers(ctx)
			if _, ok := o.failoverOf(ctx, r.inst.ID); ok {
				f, _ := o.failoverOf(ctx, r.inst.ID)
				t.Fatalf("the failover did not finish: %+v", f)
			}
			p, rep := r.members()
			if p.NodeID != "w1" || rep.Role != opmember.Replica || rep.NodeID != r.n.ctl.ID {
				t.Errorf("rows after the resume: primary %+v standby %+v", p, rep)
			}
			if rep.Fenced {
				t.Errorf("the old primary was not rejoined: %+v", rep)
			}
		})
	}
}

// The fence is the first thing written: the cursor and the old primary's row
// exist before any container is touched.
func TestFailoverFencePersistsBeforeAnything(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, false)
	o := r.n.orch
	var cursor, fenced bool
	r.before = func() { // the first container change
		_, cursor = o.failoverOf(ctx, r.inst.ID)
		p, _ := r.members()
		fenced = p.Fenced
	}
	_, err := o.Switchover(ctx, r.inst.TileID, "w1")
	must(t, err)
	if !r.fired {
		t.Fatal("no container was changed")
	}
	if !cursor || !fenced {
		t.Errorf("at the first change: cursor %v, old primary fenced %v; want both", cursor, fenced)
	}
}

// A fenced old primary is never started by the panel, and its deploy is
// refused, until it rejoins.
func TestFencedOldPrimaryStaysDown(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, true) // the primary on w1
	o := r.n.orch
	must(t, setNode(o, "w1", node.StatusOffline))
	r.ageLease(2 * time.Minute)
	r.lsn[r.n.ctl.ID] = "0/5000100"
	if _, err := o.PromoteStandby(ctx, r.inst.TileID, r.n.ctl.ID, true, "", false); err != nil {
		t.Fatal(err)
	}
	_, old := r.members()
	if !old.Fenced || old.Phase != opmember.Rewind || old.NodeID != "w1" {
		t.Fatalf("old primary = %+v, want fenced, waiting for its rewind on w1", old)
	}
	if err := opmember.MayStart(old); err == nil {
		t.Error("a fenced member may start")
	}
	o.startMembers(ctx)
	if r.saw("w1 start") {
		t.Error("the tick started something on the fenced old primary's node")
	}
	// its node comes back: it rejoins as a standby, rewound then made
	must(t, setNode(o, "w1", node.StatusOnline))
	o.resumeFailovers(ctx)
	_, old = r.members()
	if old.Fenced || old.Phase != opmember.Starting {
		t.Errorf("after its node returned: %+v", old)
	}
	r.order("w1 create stackr-rewind", "w1 create stackr-member")
}

func setNode(o *Orchestrator, id, status string) error {
	ctx := context.Background()
	n, err := o.store.Nodes.Get(ctx, id)
	if err != nil {
		return err
	}
	n.Status = status
	return o.store.Nodes.Update(ctx, n)
}

// A promote is refused while the old primary may be alive: it runs on a node
// that answers; or its node does not answer and the admin has not said it is
// gone; or has said so while the worker's lease on it has not run out.
func TestPromoteRefusedWhileOldPrimaryMayBeAlive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("running", func(t *testing.T) {
		t.Parallel()
		r := newFailRig(t, false)
		for _, gone := range []bool{false, true} {
			_, err := r.n.orch.PromoteStandby(ctx, r.inst.TileID, "w1", gone, "", false)
			if _, ok := errs.IsConflict(err); !ok || !strings.Contains(err.Error(), "running") {
				t.Errorf("promote with the primary running (gone=%v): %v", gone, err)
			}
		}
		if p, _ := r.members(); p.Fenced {
			t.Error("a refused promote fenced the primary")
		}
	})

	t.Run("unreachable, not confirmed", func(t *testing.T) {
		t.Parallel()
		r := newFailRig(t, true)
		must(t, setNode(r.n.orch, "w1", node.StatusOffline))
		_, err := r.n.orch.PromoteStandby(ctx, r.inst.TileID, r.n.ctl.ID, false, "", false)
		if _, ok := errs.IsConflict(err); !ok || !strings.Contains(err.Error(), "confirm") {
			t.Errorf("promote of an unreachable primary without the confirmation: %v", err)
		}
	})

	t.Run("lease not run out", func(t *testing.T) {
		t.Parallel()
		r := newFailRig(t, true)
		o := r.n.orch
		must(t, setNode(o, "w1", node.StatusOffline))
		r.ageLease(5 * time.Second) // the worker took the lease 5 s ago
		_, err := o.PromoteStandby(ctx, r.inst.TileID, r.n.ctl.ID, true, "", false)
		if _, ok := errs.IsConflict(err); !ok || !strings.Contains(err.Error(), "lease") {
			t.Errorf("promote inside the lease: %v", err)
		}
		if p, _ := r.members(); p.Fenced || r.saw("promote") {
			t.Error("a refused promote touched the instance")
		}
	})
}

// With the admin's word that the node is gone and the lease run out, the
// standby is promoted without ever asking the old node to stop anything.
func TestPromoteWithNodeGoneConfirmation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, true)
	o := r.n.orch
	must(t, setNode(o, "w1", node.StatusOffline))
	r.ageLease(2 * time.Minute)
	v, err := o.PromoteStandby(ctx, r.inst.TileID, r.n.ctl.ID, true, "", false)
	must(t, err)
	if v.Role != opmember.Primary {
		t.Errorf("view = %+v", v)
	}
	p, old := r.members()
	if p.NodeID != r.n.ctl.ID || !old.Fenced || old.Phase != opmember.Rewind {
		t.Errorf("rows: primary %+v, old %+v", p, old)
	}
	r.order("SELECT pg_promote", "ctl create stackr-member")
	if r.saw("w1 ") {
		t.Error("the unreachable node was asked something")
	}
	if _, ok := o.failoverOf(ctx, r.inst.ID); ok {
		t.Error("the cursor outlived the promote")
	}
}

// A forgotten node is no way round the fence: its last recorded binding and
// lease deadline outlive its row, and without a record only the typed
// override promotes.
func TestPromoteWhenOldNodeForgotten(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, true)
	o := r.n.orch
	inst := r.inst
	must(t, o.store.Nodes.Delete(ctx, "w1")) // forgotten, its lease record stays
	if _, ok := o.leaseRecOf(ctx, inst.ID, "w1"); !ok {
		t.Fatal("forgetting the node dropped its lease record")
	}
	err := o.mayPromote(ctx, r.tile(), inst, "w1", false, false)
	if _, ok := errs.IsConflict(err); !ok || !strings.Contains(err.Error(), "lease runs out") {
		t.Errorf("forgotten node inside its lease: %v", err)
	}
	r.ageLease(2 * time.Minute)
	if err := o.mayPromote(ctx, r.tile(), inst, "w1", false, false); err != nil {
		t.Errorf("forgotten node, lease run out: %v", err)
	}
	// no record: refused, unless the admin typed the override
	must(t, o.store.Settings.Delete(ctx, leaseKey(inst.ID, "w1")))
	err = o.mayPromote(ctx, r.tile(), inst, "w1", false, false)
	if _, ok := errs.IsConflict(err); !ok || !strings.Contains(err.Error(), PoweredOffPhrase) {
		t.Errorf("no lease record: %v", err)
	}
	if err := o.mayPromote(ctx, r.tile(), inst, "w1", false, true); err != nil {
		t.Errorf("with the override: %v", err)
	}
	if _, err := o.PromoteStandby(ctx, r.inst.TileID, r.n.ctl.ID, false, "powered off", false); err == nil {
		t.Error("a wrong phrase was accepted")
	}
}

// A promote of an unreachable node with no recorded binding is refused, and
// goes through on the typed override, which the cursor records.
func TestPromoteNeedsLeaseProofOrOverride(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, true)
	o := r.n.orch
	must(t, setNode(o, "w1", node.StatusOffline))
	must(t, o.store.Settings.Delete(ctx, leaseKey(r.inst.ID, "w1")))
	_, err := o.PromoteStandby(ctx, r.inst.TileID, r.n.ctl.ID, true, "", false)
	if _, ok := errs.IsConflict(err); !ok || !strings.Contains(err.Error(), "no recorded lease") {
		t.Fatalf("no proof: %v", err)
	}
	if p, _ := r.members(); p.Fenced {
		t.Error("a refused promote fenced the primary")
	}
	_, err = o.PromoteStandby(ctx, r.inst.TileID, r.n.ctl.ID, true, PoweredOffPhrase, false)
	must(t, err)
	if p, _ := r.members(); p.NodeID != r.n.ctl.ID {
		t.Errorf("primary after the override: %+v", p)
	}
}

// PITR cannot follow a relocated primary yet: a failover of such an instance
// is refused, nothing fenced, nothing touched.
func TestSwitchoverWithPITRFollowsThePrimary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, false)
	o := r.n.orch
	inst, err := o.operated.SetPITR(ctx, r.inst, 7)
	must(t, err)
	dv, err := o.dataVolume(ctx, r.tile())
	must(t, err)
	o.baseSchedule(ctx, dv)
	if ss, _ := o.backups.Schedules(ctx, dv.ID); len(ss) == 0 {
		t.Fatal("no base schedule to follow")
	}
	pre, _ := o.pitrState(ctx, inst.ID)
	pre.LastSegment = "000000010000000000000002"
	o.savePITRState(ctx, inst.ID, pre)
	_, err = o.Switchover(ctx, r.inst.TileID, "w1")
	must(t, err)
	p, rep := r.members()

	// 1. the promoted primary archives: same flags, an archive volume on its node
	var spec docker.ContainerSpec
	for _, sp := range r.specs {
		if sp.Labels[tile.LabelTile] != "" && sp.Labels[LabelMember] == p.ID {
			spec = sp
		}
	}
	if !slices.Contains(spec.Cmd, "archive_mode=on") {
		t.Errorf("promoted command does not archive: %v", spec.Cmd)
	}
	if want := mflow.MemberArchiveVolume(p) + ":" + mflow.PGArchiveDir; !slices.Contains(spec.Volumes, want) {
		t.Errorf("volumes = %v, want %s", spec.Volumes, want)
	}
	if !slices.ContainsFunc(r.n.wFake.Created, func(c dockerfake.VolumeCreate) bool { return c.Name == mflow.MemberArchiveVolume(p) }) {
		t.Error("the archive volume was not made on the primary's node")
	}

	// 3. the data volume row, and with it every schedule and the archive root, follows
	dv2, err := o.dataVolume(ctx, r.tile())
	must(t, err)
	if dv2.ID != dv.ID || dv2.Name != p.Volume || dv2.Name == dv.Name || dv2.NodeID != "w1" || o.volumeNode(ctx, dv.ID) != "w1" {
		t.Errorf("data row = %+v, want the primary's volume on w1", dv2)
	}
	if ss, _ := o.backups.Schedules(ctx, dv.ID); len(ss) == 0 {
		t.Error("the schedules left the volume")
	}
	if rep.Volume != dv.Name {
		t.Errorf("old primary holds %q, want its old volume %q", rep.Volume, dv.Name)
	}

	// 4. the epoch waits for the new primary's archiver
	st, _ := o.pitrState(ctx, inst.ID)
	arch := st.ArchSince // the epoch carries through the failover
	if !st.Paused || arch.IsZero() || len(st.Marks) != 1 || st.Marks[0].Seg != pre.LastSegment || st.PausedSeg == "" || !o.hasProblem(ctx, "alert.pitr.lag."+inst.ID) {
		t.Fatalf("state after failover = %+v, want paused with a problem", st)
	}
	r.mu.Lock()
	r.notArchived = true
	r.mu.Unlock()
	o.pitrShip(ctx, inst)
	if st, _ = o.pitrState(ctx, inst.ID); !st.Paused || !st.BaseTriedAt.IsZero() {
		t.Fatalf("resumed before an archived segment was seen: %+v", st)
	}
	r.mu.Lock()
	r.notArchived = false
	r.mu.Unlock()
	o.pitrShip(ctx, inst)
	st, _ = o.pitrState(ctx, inst.ID)
	if st.Paused || !st.ArchSince.Equal(arch) || st.BaseTriedAt.IsZero() {
		t.Errorf("state after archiving = %+v, want the epoch kept and a base queued", st)
	}
	if o.hasProblem(ctx, "alert.pitr.lag."+inst.ID) {
		t.Error("the paused problem outlived the pause")
	}
	// 2. the shipper read the archive on the primary's node
	o.pitrShip(ctx, inst)
	if !r.saw("w1 exec ls -1 " + mflow.PGArchiveDir) {
		t.Error("the archive was not read on the primary's node")
	}
}

// The pause is written in the swap's own transaction: a panel that dies right
// after the swap leaves an unarmed pause that no tick resumes, and nothing
// ships into the old epoch. The route step arms it.
func TestSwapHoldsTheArchiveUntilRouteArms(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, false)
	o := r.n.orch
	inst, err := o.operated.SetPITR(ctx, r.inst, 7)
	must(t, err)
	o.failCrashAfter = stepSwap
	if _, err := o.Switchover(ctx, r.inst.TileID, "w1"); err == nil {
		t.Fatal("the crash did not stop the run")
	}
	st, _ := o.pitrState(ctx, inst.ID)
	arch := st.ArchSince
	if !st.Paused || st.PausedSeg != "" || arch.IsZero() {
		t.Fatalf("state after the swap = %+v, want an unarmed pause that keeps the epoch", st)
	}
	o.pitrShip(ctx, inst) // a tick between swap and route
	if st, _ = o.pitrState(ctx, inst.ID); !st.Paused || st.PausedSeg != "" || !st.ArchSince.Equal(arch) || !st.BaseTriedAt.IsZero() {
		t.Errorf("tick during the failover = %+v, want it still unarmed", st)
	}
	if r.saw("w1 exec ls -1 " + mflow.PGArchiveDir) {
		t.Error("the tick shipped while paused")
	}
	o.failCrashAfter = ""
	o.resumeFailovers(ctx)
	if st, _ = o.pitrState(ctx, inst.ID); !st.Paused || st.PausedSeg == "" {
		t.Errorf("state after route = %+v, want it armed", st)
	}
	// an unarmed pause never resumes, even with everything archived
	must(t, o.pitrHold(ctx, r.tile(), inst, ""))
	st, _ = o.pitrState(ctx, inst.ID)
	dv, _ := o.dataVolume(ctx, r.tile())
	o.pitrResume(ctx, r.tile(), dv, &st, time.Now())
	if !st.Paused {
		t.Error("an unarmed pause resumed")
	}
}

// A backup or restore holding the data volume keeps the swap from moving its
// row under it; the step fails and the tick retries it.
func TestSwapWaitsForTheVolumeJob(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, false)
	o := r.n.orch
	dv, err := o.dataVolume(ctx, r.tile())
	must(t, err)
	j, err := o.jobRows.Create(ctx, string(kindBackup), []string{"volume:" + dv.ID}, "{}", nil, t.TempDir())
	must(t, err)
	_, err = o.jobRows.Start(ctx, j)
	must(t, err)
	if _, err := o.Switchover(ctx, r.inst.TileID, "w1"); err == nil || !strings.Contains(err.Error(), "holds the data volume") {
		t.Fatalf("switchover under a running backup: %v", err)
	}
	if p, _ := r.members(); p.NodeID != "" {
		t.Errorf("primary row = %+v, swapped under a running backup", p)
	}
	if v, _ := o.dataVolume(ctx, r.tile()); v.Name != dv.Name {
		t.Error("the data row moved under a running backup")
	}
	must(t, o.jobRows.Finish(ctx, j, job.Done, ""))
	o.resumeFailovers(ctx)
	if p, _ := r.members(); p.NodeID != "w1" {
		t.Errorf("primary row = %+v, want the swap once the backup is done", p)
	}
}

// Back home: the data row names the original volume again, on the env's node.
func TestSwitchoverBackHomeMovesBackupsBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, false)
	o := r.n.orch
	inst, err := o.operated.SetPITR(ctx, r.inst, 7)
	must(t, err)
	dv, err := o.dataVolume(ctx, r.tile())
	must(t, err)
	_, err = o.Switchover(ctx, r.inst.TileID, "w1")
	must(t, err)
	_, rep := r.members()
	_, err = o.members.SetPhase(ctx, rep, opmember.Streaming)
	must(t, err)
	r.mu.Lock()
	r.lsn["ctl"] = "0/5000100"
	r.mu.Unlock()
	_, err = o.Switchover(ctx, r.inst.TileID, r.n.ctl.ID)
	must(t, err)
	p, rep := r.members()
	if p.NodeID != r.n.ctl.ID {
		t.Fatalf("primary row = %+v, want it home", p)
	}
	dv2, err := o.dataVolume(ctx, r.tile())
	must(t, err)
	if dv2.Name != dv.Name || dv2.NodeID != "" || o.volumeNode(ctx, dv.ID) != r.n.ctl.ID {
		t.Errorf("data row = %+v, want the original volume %q on the env's node", dv2, dv.Name)
	}
	if rep.NodeID != "w1" {
		t.Errorf("standby = %+v, want the one on w1", rep)
	}
	if st, _ := o.pitrState(ctx, inst.ID); !st.Paused {
		t.Errorf("state = %+v, want paused again after the second failover", st)
	}
}

// A promote with PITR on also keeps archiving: no drain (the old node is
// gone), the data row moves to the promoted primary, the epoch is paused.
func TestPromoteWithPITRKeepsArchiving(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, true)
	o := r.n.orch
	inst, err := o.operated.SetPITR(ctx, r.inst, 7)
	must(t, err)
	must(t, setNode(o, "w1", node.StatusOffline))
	r.ageLease(2 * time.Minute)
	_, err = o.PromoteStandby(ctx, r.inst.TileID, r.n.ctl.ID, true, "", false)
	must(t, err)
	p, _ := r.members()
	dv, err := o.dataVolume(ctx, r.tile())
	must(t, err)
	if dv.Name != p.Volume || dv.NodeID != r.n.ctl.ID {
		t.Errorf("data row = %+v, want the promoted primary's volume on the controller", dv)
	}
	if st, _ := o.pitrState(ctx, inst.ID); !st.Paused {
		t.Errorf("state = %+v, want paused", st)
	}
}

// Without --to, the standby that replayed the most is promoted.
func TestFailoverTargetByLSN(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, false)
	o := r.n.orch
	must(t, o.store.Nodes.Create(ctx, store.Node{ID: "w2", Name: "w2", Slug: "w2", Role: node.Worker, Tags: "[]", RangeCIDR: "10.210.0.0/16",
		Status: node.StatusOnline, CreatedAt: time.Now()}))
	w2 := &sim{Docker: &subnetFake{Fake: r.n.wFake, nets: map[string]bool{}}, rig: r, node: "w2"}
	o.docker.(*DockerRouter).Set("w2", w2)
	rep2, err := o.AddStandby(ctx, r.inst.TileID, "w2")
	must(t, err)
	o.vipAsync.Wait()
	_, err = o.members.SetPhase(ctx, rep2, opmember.Streaming)
	must(t, err)
	r.lsn["w1"], r.lsn["w2"] = "0/5000100", "0/7000000"
	ms, _ := o.members.List(ctx, r.inst.ID)
	got, err := o.failoverTarget(ctx, r.tile(), ms, "", foSwitchover)
	must(t, err)
	if got.ID != rep2.ID {
		t.Errorf("target = %s, want the standby on w2 (the most advanced)", got.NodeID)
	}
	r.lsn["w1"] = "0/9000000"
	if got, err = o.failoverTarget(ctx, r.tile(), ms, "", foSwitchover); err != nil || got.NodeID != "w1" {
		t.Errorf("target = %v %v, want w1 once it is ahead", got.NodeID, err)
	}
	// a named node wins over the LSN, and must hold a standby
	if got, err = o.failoverTarget(ctx, r.tile(), ms, "w2", foSwitchover); err != nil || got.NodeID != "w2" {
		t.Errorf("named target = %v %v", got.NodeID, err)
	}
	if _, err = o.failoverTarget(ctx, r.tile(), ms, r.n.ctl.ID, foSwitchover); err == nil {
		t.Error("a node with no standby was accepted")
	}
}

// A relocated primary is the tile's own again when its env runs on the
// primary's node; anywhere else the instance tile's deploy is refused.
func TestPlacementAdoptsPromotedPrimary(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, false)
	o := r.n.orch
	oldVol, err := o.dataVolume(ctx, r.tile())
	must(t, err)
	_, err = o.Switchover(ctx, r.inst.TileID, "w1")
	must(t, err)
	p, _ := r.members()

	err = o.adoptPrimary(ctx, r.tile())
	if _, ok := errs.IsConflict(err); !ok {
		t.Fatalf("deploy on the env's node with the primary on w1: %v, want a conflict", err)
	}
	if _, err := o.engines.Container(ctx, r.tile()); err == nil {
		t.Error("the instance tile's container was built while its primary is elsewhere")
	}

	must(t, o.store.Environments.SetNode(ctx, r.n.dev.ID, "w1")) // the env follows
	must(t, o.adoptPrimary(ctx, r.tile()))
	p2, _ := r.members()
	if p2.NodeID != "" || p2.Container != "" || p2.Volume != "" {
		t.Errorf("primary row = %+v, want the tile's own again", p2)
	}
	v, err := o.dataVolume(ctx, r.tile())
	must(t, err)
	if v.Name != p.Volume || v.Name == oldVol.Name || v.NodeID != "" {
		t.Errorf("data volume names %q, want the member's %q", v.Name, p.Volume)
	}
	if !r.saw("w1 remove " + p.Container) {
		t.Error("the member's container was not removed before the tile's own is made")
	}
	if _, ok := o.primaryReroute(ctx, r.tile()); ok {
		t.Error("still rerouted after the adoption")
	}
}

// Adopting a primary of an instance with PITR on is an archive handoff: the
// archive is held (unarmed) and the member's and the tile's old spools go, so
// the tile's container starts on an empty one.
func TestAdoptHandsOffTheArchive(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, false)
	o := r.n.orch
	inst, err := o.operated.SetPITR(ctx, r.inst, 7)
	must(t, err)
	wal, _, err := o.volumes.Declare(ctx, volume.Scope{Kind: "env", ID: r.tile().EnvironmentID}, mflow.ArchiveSlug(r.tile().Slug), &inst.ID)
	must(t, err)
	_, err = o.Switchover(ctx, r.inst.TileID, "w1")
	must(t, err)
	p, _ := r.members()
	must(t, o.store.Environments.SetNode(ctx, r.n.dev.ID, "w1"))
	must(t, o.adoptPrimary(ctx, r.tile()))
	st, _ := o.pitrState(ctx, inst.ID)
	if !st.Paused || st.PausedSeg != "" || st.ArchSince.IsZero() {
		t.Errorf("state after adoption = %+v, want an unarmed pause that keeps the epoch", st)
	}
	if !sawCall(r.n.wFake, "RemoveVolume("+mflow.MemberArchiveVolume(p)+")") {
		t.Error("the member's spool was kept")
	}
	if !sawCall(r.n.wFake, "RemoveVolume("+wal.Name+")") {
		t.Error("the tile's own spool was not cleared")
	}
}

// A lease binds the running primaries of instances that have a standby, on
// workers; an instance with no standby, and the controller's node, are never
// bound. A worker that had some and has none is told to drop them, and that
// is known from the persisted records, not from memory.
func TestLeaseBindsOnlyPrimariesWithAStandby(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, true) // the primary on w1
	o := r.n.orch

	if got := o.leaseContainers(ctx); len(got["w1"]) != 1 || got["w1"][0] != "pg" {
		t.Fatalf("lease containers = %v, want the primary on w1", got)
	}
	// adding the standby already pushed and recorded it, synchronously
	if l := r.pushed["w1"]; l.Seconds != leaseSeconds || !slices.Equal(l.Containers, []string{"pg"}) {
		t.Fatalf("pushed at the first standby: %+v", l)
	}
	if rec, ok := o.leaseRecOf(ctx, r.inst.ID, "w1"); !ok || !slices.Equal(rec.Containers, []string{"pg"}) {
		t.Fatalf("lease record = %+v %v", rec, ok)
	}
	delete(r.pushed, "w1")
	o.pushLeases(ctx)
	if l := r.pushed["w1"]; len(l.Containers) != 1 {
		t.Errorf("pushed %+v", l)
	}
	if _, ok := r.pushed[r.n.ctl.ID]; ok {
		t.Error("the controller's node got a lease")
	}
	if o.leaseWait(ctx, "w1") <= 0 {
		t.Error("no wait counted from a push that was just taken")
	}

	// the standby goes: the instance is single again, the lease is cleared
	_, rep := r.members()
	must(t, o.store.OperatedMembers.Delete(ctx, rep.ID))
	if got := o.leaseContainers(ctx); len(got) != 0 {
		t.Errorf("a single instance is bound: %v", got)
	}
	o.pushLeases(ctx)
	if l, ok := r.pushed["w1"]; !ok || len(l.Containers) != 0 {
		t.Errorf("the lease was not cleared with an explicit empty list: %+v", l)
	}
	if _, ok := o.leaseRecOf(ctx, r.inst.ID, "w1"); ok {
		t.Error("the record outlived the cleared lease")
	}
}

// A collection that failed keeps the previous lease: nothing is pushed, an
// empty list least of all.
func TestLeaseCollectionErrorPushesNothing(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, true)
	o := r.n.orch
	delete(r.pushed, "w1")
	r.mu.Lock()
	r.listErr = errors.New("docker down") // the primaries cannot be listed
	r.mu.Unlock()
	o.pushLeases(ctx)
	if len(r.pushed) != 0 {
		t.Errorf("pushed after a failed collection: %+v", r.pushed)
	}
	if _, ok := o.leaseRecOf(ctx, r.inst.ID, "w1"); !ok {
		t.Error("the record was dropped by a failed collection")
	}
}

// The first standby is only added once the worker took the primary's lease;
// a push that fails undoes the add.
func TestAddStandbyNeedsTheLeaseAck(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, true)
	o := r.n.orch
	_, rep := r.members()
	must(t, o.RemoveStandby(ctx, rep.ID))
	r.leaseErr = errors.New("worker unreachable")
	if _, err := o.AddStandby(ctx, r.inst.TileID, r.n.ctl.ID); err == nil {
		t.Error("a standby was added without the primary's lease")
	}
}

// A primary container that comes up with standbys is leased before it is
// trusted; if the worker does not take the lease it is stopped.
func TestReplacedPrimaryIsLeasedOrStopped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, true)
	o := r.n.orch
	delete(r.pushed, "w1")
	o.leaseOnPrimaryUp(ctx, r.tile(), "pg")
	if l := r.pushed["w1"]; len(l.Containers) != 1 {
		t.Fatalf("no lease pushed for the replaced primary: %+v", r.pushed)
	}
	if r.saw("w1 stop pg") {
		t.Error("a leased primary was stopped")
	}
	r.leaseErr = errors.New("worker unreachable")
	o.leaseOnPrimaryUp(ctx, r.tile(), "pg")
	if !r.saw("w1 stop pg") {
		t.Error("a primary with no lease was left running")
	}
}

// A failover is not complete until the new primary's worker took its lease:
// a failed push keeps the cursor, the tick finishes it later.
func TestFailoverCompletesOnlyWithTheNewPrimaryLeased(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, false) // the primary on the controller, the standby on w1
	o := r.n.orch
	r.leaseErr = errors.New("worker unreachable")
	if _, err := o.Switchover(ctx, r.inst.TileID, "w1"); err == nil {
		t.Fatal("the switchover completed with the new primary unleased")
	}
	if _, ok := o.failoverOf(ctx, r.inst.ID); !ok {
		t.Fatal("the cursor was dropped")
	}
	r.leaseErr = nil
	o.resumeFailovers(ctx)
	if _, ok := o.failoverOf(ctx, r.inst.ID); ok {
		t.Error("the failover did not finish once the lease was taken")
	}
	if l := r.pushed["w1"]; len(l.Containers) != 1 {
		t.Errorf("new primary's lease = %+v", l)
	}
}

// The primary-down problem opens once it has stayed down and clears when it
// runs again; a primary the user stopped is not an outage.
func TestPrimaryDownProblem(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, false)
	o := r.n.orch
	old := downAfter
	downAfter = 0
	t.Cleanup(func() { downAfter = old })
	key := "alert." + AlertPGPrimaryDown + "." + r.inst.ID

	r.sims["ctl"].set("pg", "exited")
	o.observePrimaries(ctx)
	if !o.hasProblem(ctx, key) {
		t.Fatal("no problem for a primary that is down with a standby")
	}
	if href, text, ok := o.problemLink(ctx, strings.TrimPrefix(key, "alert."), map[string]bool{}); !ok || text != "Promote a standby" || !strings.Contains(href, "drawer=") {
		t.Errorf("link = %q %q %v", href, text, ok)
	}
	r.sims["ctl"].set("pg", "running")
	o.observePrimaries(ctx)
	if o.hasProblem(ctx, key) {
		t.Error("the problem stayed after the primary came back")
	}
	p, _ := r.members()
	_, err := o.members.SetState(ctx, p, opmember.Stopped)
	must(t, err)
	r.sims["ctl"].set("pg", "exited")
	o.observePrimaries(ctx)
	if o.hasProblem(ctx, key) {
		t.Error("a primary stopped on purpose is an outage")
	}
}

// The standby's start script empties a data directory without standby.signal;
// a promoted one has the completion marker and must be left alone.
func TestStandbyScriptNeverEmptiesAPromotedDirectory(t *testing.T) {
	t.Parallel()
	r := newFailRig(t, false)
	spec, err := r.n.orch.engines.StandbySpec(context.Background(), r.tile(), mustMember(t, r), "alias:10.0.0.1")
	must(t, err)
	script := spec.Cmd[2]
	guard := strings.Index(script, "was promoted")
	wipe := strings.Index(script, "find \"$d\" -mindepth 1 -delete")
	if guard < 0 || wipe < 0 || guard > wipe {
		t.Errorf("the promoted-directory guard (%d) must come before the wipe (%d)", guard, wipe)
	}
	_ = mflow.ReplPassfile
}

func mustMember(t *testing.T, r *failRig) InstanceMember {
	_, rep := r.members()
	return rep
}

// A consumer on the old primary's node reaches the new primary across the
// mesh: the new primary's node accepts its env (linkFlows only walked the
// envs on that node, which do not hold the instance's tile).
func TestSwitchoverAcceptsConsumersOfTheOldNode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, false)
	o := r.n.orch
	from := "shop:dev:" + r.tile().Slug
	sl, err := o.tiles.Create(ctx, store.Tile{StackID: r.n.stack, EnvironmentID: r.n.dev.ID, Name: "db", Kind: tile.Slice, ProvisionFrom: &from})
	must(t, err)
	app, err := o.tiles.Create(ctx, store.Tile{StackID: r.n.stack, EnvironmentID: r.n.dev.ID, Name: "app", Kind: tile.Image, ImageRef: "x"})
	must(t, err)
	p := store.Provision{ID: uuid.NewString(), TileID: sl.ID, InstanceID: r.inst.ID, DBName: "a", DBUser: "a", DBPassword: "x", CreatedAt: time.Now()}
	must(t, r.n.st.Provisions.Create(ctx, p))
	must(t, r.n.st.Bindings.Create(ctx, store.Binding{ID: uuid.NewString(), ProvisionID: p.ID, ConsumerTileID: app.ID,
		Access: tile.Write, DBUser: "b", DBPassword: "y", Outputs: "{}", CreatedAt: time.Now()}))
	_, err = o.Switchover(ctx, r.inst.TileID, "w1")
	must(t, err)
	w1, err := r.n.st.Nodes.Get(ctx, "w1")
	must(t, err)
	if !slices.ContainsFunc(o.linkFlows(ctx, w1), func(f vip.Flow) bool { return !f.Out && f.Port == 5432 && f.Src.String() == "10.208.0.0/24" }) {
		t.Errorf("w1 flows = %+v, want an accept for the controller's env", o.linkFlows(ctx, w1))
	}
}

// The request's context ends (hamr's 30 s timeout) long before a switchover
// does; the failover and its answer do not depend on it.
func TestSwitchoverSurvivesTheRequestContext(t *testing.T) {
	t.Parallel()
	r := newFailRig(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	v, err := r.n.orch.Switchover(ctx, r.inst.TileID, "w1")
	if err != nil || v.Node != "w1" {
		t.Errorf("switchover = %+v %v", v, err)
	}
}

// A node that went silent is marked offline minutes later. Until then a
// promote the admin confirmed must not hang on it: its Docker does not
// answer, which is what "unreachable" means.
func TestPromoteWhenTheNodeIsSilentButNotYetOffline(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, true)
	o := r.n.orch
	r.mu.Lock()
	r.downNode = "w1"
	r.mu.Unlock()
	r.ageLease(2 * time.Minute)
	_, err := o.PromoteStandby(ctx, r.inst.TileID, r.n.ctl.ID, false, "", false)
	if _, ok := errs.IsConflict(err); !ok || !strings.Contains(err.Error(), "confirm") {
		t.Fatalf("not confirmed: %v", err)
	}
	if _, err := o.PromoteStandby(ctx, r.inst.TileID, r.n.ctl.ID, true, "", false); err != nil {
		t.Fatal(err)
	}
	if p, _ := r.members(); p.NodeID != r.n.ctl.ID {
		t.Errorf("primary after the promote: %+v", p)
	}
}

// A switchover stuck before the promote (the standby never replays the
// checkpoint) is aborted: the old primary is unfenced and started again.
func TestAbortStuckSwitchover(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, false)
	o := r.n.orch
	r.lsn["w1"] = "0/1000000" // behind the shutdown checkpoint
	if _, err := o.Switchover(ctx, r.inst.TileID, "w1"); err == nil {
		t.Fatal("the switchover did not get stuck")
	}
	if fo, _, _ := o.FailoverOptionsOf(ctx, r.inst.TileID); !fo.CanAbort {
		t.Error("the drawer is not offered the abort")
	}
	if p, _ := r.members(); !p.Fenced {
		t.Fatal("the old primary is not fenced while stuck")
	}
	r.mu.Lock()
	r.events = nil
	r.mu.Unlock()
	must(t, o.AbortSwitchover(ctx, r.inst.TileID))
	if _, ok := o.failoverOf(ctx, r.inst.ID); ok {
		t.Error("the cursor outlived the abort")
	}
	if p, _ := r.members(); p.Fenced || p.NodeID != "" {
		t.Errorf("primary row = %+v, want unfenced and in place", p)
	}
	if !r.saw("ctl start pg") {
		t.Error("the old primary was not started again")
	}
	if r.promoted["w1"] {
		t.Error("the standby was promoted")
	}
}

// Past the promote step, or for a promote, there is nothing to abort.
func TestAbortRefusedAfterPromote(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, false)
	o := r.n.orch
	if err := o.AbortSwitchover(ctx, r.inst.TileID); !conflict(err) {
		t.Errorf("no failover: err = %v, want a conflict", err)
	}
	o.failCrashAfter = stepPromote
	if _, err := o.Switchover(ctx, r.inst.TileID, "w1"); err == nil {
		t.Fatal("the crash did not stop the run")
	}
	if err := o.AbortSwitchover(ctx, r.inst.TileID); !conflict(err) {
		t.Errorf("after the promote: err = %v, want a conflict", err)
	}
	if _, ok := o.failoverOf(ctx, r.inst.ID); !ok {
		t.Error("the refused abort removed the cursor")
	}
	// a cursor still before the promote, but the standby is out of recovery
	f, _ := o.failoverOf(ctx, r.inst.ID)
	f.Step = stepCheck
	must(t, o.saveFailover(ctx, r.inst.ID, f))
	if err := o.AbortSwitchover(ctx, r.inst.TileID); !conflict(err) {
		t.Errorf("standby promoted: err = %v, want a conflict", err)
	}
}

// A standby is fenced and unfenced by hand: fenced it is stopped and out of
// the read pool; the primary and a standby that needs a rewind are refused.
func TestFenceStandbyByHand(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, false)
	o := r.n.orch
	_, rep := r.members()
	v, err := o.FenceStandbyOf(ctx, r.inst.TileID, rep.ID, true)
	must(t, err)
	if !v.Fenced || v.ReadPool || v.State != "fenced" {
		t.Errorf("view = %+v, want fenced and out of the read pool", v)
	}
	if !r.saw("w1 stop") {
		t.Error("the fenced standby was not stopped")
	}
	o.startMembers(ctx)
	if r.saw("w1 start") {
		t.Error("the tick started a fenced standby")
	}
	v, err = o.FenceStandbyOf(ctx, r.inst.TileID, rep.ID, false)
	must(t, err)
	if v.Fenced {
		t.Errorf("view = %+v, want unfenced", v)
	}
	p, _ := r.members()
	if _, err := o.FenceStandbyOf(ctx, r.inst.TileID, p.ID, true); !errors.Is(err, errs.ErrNotFound) {
		t.Errorf("fence of the primary: %v, want not found", err)
	}
	_, err = o.members.SetPhase(ctx, rep, opmember.Rewind)
	must(t, err)
	if _, err := o.FenceStandbyOf(ctx, r.inst.TileID, rep.ID, false); !conflict(err) {
		t.Errorf("unfence of a standby that needs a rewind: %v, want a conflict", err)
	}
}

// Each running standby gets its own pooler on its node under the read pooler
// alias; nothing is written to the standby; the ini follows the slices by a
// reload; it stops with a fence and goes with the standby.
func TestStandbyPooler(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, false)
	o := r.n.orch
	_, err := o.operated.SetPoolerPassword(ctx, r.inst, "pw")
	must(t, err)
	_, rep := r.members()
	// the standby's own container, so the pooler has something to sit beside
	r.sims["w1"].cs = append(r.sims["w1"].cs, simCtr{Container: docker.Container{ID: "sb", State: "running", Labels: map[string]string{LabelMember: rep.ID}}})

	o.syncStandbyPoolers(ctx)
	var spec docker.ContainerSpec
	for _, s := range r.specs {
		if s.Labels[mflow.LabelMemberPooler] == rep.ID {
			spec = s
		}
	}
	if spec.Name == "" {
		t.Fatalf("no pooler was made; log:\n%s", strings.Join(r.events, "\n"))
	}
	if got := spec.Networks[0].Aliases; len(got) != 1 || !strings.HasSuffix(got[0], "-read-pooler") {
		t.Errorf("aliases = %v, want the read pooler alias only", got)
	}
	if !strings.Contains(spec.Env[0], "-read port=5432") {
		t.Errorf("the pooler does not dial the standby by the read alias: %s", spec.Env[0])
	}
	if !r.saw("w1 start stackr-") {
		t.Error("the pooler was not started")
	}
	for _, e := range r.events {
		if strings.HasPrefix(e, "w1 exec") && (strings.Contains(e, "CREATE") || strings.Contains(e, "ALTER") || strings.Contains(e, "GRANT")) {
			t.Errorf("a write reached the standby: %s", e)
		}
	}
	// a pass with nothing changed does nothing more; a changed ini is reloaded
	must(t, o.syncStandbyPooler(ctx, rep))
	if n := len(r.specs); n != 1 {
		t.Errorf("%d containers made, want one", n)
	}
	if !r.saw("kill -HUP 1") { // the sim's pooler answers no ini, so it is stale
		t.Error("a stale ini was not reloaded")
	}
	// fenced: stopped, and not started again
	r.mu.Lock()
	r.events = nil
	r.mu.Unlock()
	must(t, o.FenceMember(ctx, rep.ID, true))
	if !r.saw("w1 stop stackr-") {
		t.Error("the fenced standby's pooler was not stopped")
	}
	o.syncStandbyPoolers(ctx)
	if r.saw("w1 start") {
		t.Error("a fenced standby's pooler was started")
	}
	// removed with the standby
	nctx := docker.WithNode(ctx, "w1")
	must(t, o.removePoolers(nctx, rep))
	if cs, _ := o.poolerContainers(nctx, rep.ID); len(cs) != 0 {
		t.Errorf("pooler left behind: %v", cs)
	}
}

// The primary's pooler follows the primary: after a switchover it runs on the
// new primary's node and is gone from the old one, and again after switching
// back home.
func TestPrimaryPoolerFollowsTheSwitchover(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, false)
	o := r.n.orch
	_, err := o.operated.SetPoolerPassword(ctx, r.inst, "pw")
	must(t, err)
	pt := r.tile()
	r.sims["ctl"].cs = append(r.sims["ctl"].cs, simCtr{Container: docker.Container{ID: "pool0", Name: "stackr-pooler", State: "running",
		Labels: map[string]string{tile.LabelTile: pt.ID, tile.LabelRole: mflow.PoolerRole}}})
	poolers := func(node string) []docker.Container {
		cs, _ := r.sims[node].List(ctx, map[string]string{tile.LabelTile: pt.ID, tile.LabelRole: mflow.PoolerRole})
		return cs
	}
	_, err = o.Switchover(ctx, r.inst.TileID, "w1")
	must(t, err)
	if len(poolers("w1")) != 1 || poolers("w1")[0].State != "running" {
		t.Errorf("poolers on the new primary's node: %v", poolers("w1"))
	}
	if len(poolers("ctl")) != 0 {
		t.Errorf("the old node's primary pooler is still there: %v", poolers("ctl"))
	}
	_, rep := r.members()
	_, err = o.members.SetPhase(ctx, rep, opmember.Streaming)
	must(t, err)
	r.mu.Lock()
	r.lsn["ctl"] = "0/5000100"
	r.mu.Unlock()
	_, err = o.Switchover(ctx, r.inst.TileID, r.n.ctl.ID)
	must(t, err)
	if len(poolers("ctl")) != 1 || len(poolers("w1")) != 0 {
		t.Errorf("after switching back: ctl %v, w1 %v", poolers("ctl"), poolers("w1"))
	}
}

// An abort fails closed: a standby that cannot be asked is not shown
// unpromoted, so the cursor stays.
func TestAbortFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, false)
	o := r.n.orch
	r.lsn["w1"] = "0/1000000"
	if _, err := o.Switchover(ctx, r.inst.TileID, "w1"); err == nil {
		t.Fatal("the switchover did not get stuck")
	}
	r.sims["w1"].mu.Lock()
	r.sims["w1"].cs = nil // the standby's container is gone: it cannot be asked
	r.sims["w1"].mu.Unlock()
	if err := o.AbortSwitchover(ctx, r.inst.TileID); !conflict(err) {
		t.Errorf("abort with an unreadable standby: %v, want a conflict", err)
	}
	if _, ok := o.failoverOf(ctx, r.inst.ID); !ok {
		t.Error("the cursor went although the standby could not be asked")
	}
	if p, _ := r.members(); !p.Fenced {
		t.Error("the old primary was unfenced")
	}
}

// The standby's pooler is gone before pg_promote, and a failed stop of a
// fence is returned, retried by fencing again, and fences take the lock.
func TestPoolerStopsBeforePromoteAndFenceRetriesStop(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, false)
	o := r.n.orch
	_, err := o.operated.SetPoolerPassword(ctx, r.inst, "pw")
	must(t, err)
	_, rep := r.members()
	r.sims["w1"].cs = append(r.sims["w1"].cs, simCtr{Container: docker.Container{ID: "sb", State: "running", Labels: map[string]string{LabelMember: rep.ID}}})
	o.syncStandbyPoolers(ctx)
	if cs, _ := o.poolerContainers(docker.WithNode(ctx, "w1"), rep.ID); len(cs) != 1 {
		t.Fatalf("no standby pooler: %v", cs)
	}
	_, err = o.Switchover(ctx, r.inst.TileID, "w1")
	must(t, err)
	r.order("w1 remove stackr-", "SELECT pg_promote")

	// a stop that fails: the fence stays, the error is returned, fencing again retries
	r2 := newFailRig(t, false)
	o2 := r2.n.orch
	_, rep2 := r2.members()
	r2.sims["w1"].cs = append(r2.sims["w1"].cs, simCtr{Container: docker.Container{ID: "sb2", Name: "sb2", State: "running", Labels: map[string]string{LabelMember: rep2.ID}}})
	r2.mu.Lock()
	r2.stopErr = errors.New("daemon busy")
	r2.mu.Unlock()
	if _, err := o2.FenceStandbyOf(ctx, r2.inst.TileID, rep2.ID, true); err == nil {
		t.Fatal("a failed stop was swallowed")
	}
	if _, cur := r2.members(); !cur.Fenced {
		t.Error("the fence was not kept")
	}
	r2.mu.Lock()
	r2.stopErr = nil
	r2.mu.Unlock()
	if _, err := o2.FenceStandbyOf(ctx, r2.inst.TileID, rep2.ID, true); err != nil {
		t.Fatalf("the retry: %v", err)
	}
}

// A state row that cannot be read stops the swap and the hold: they never
// write a fresh window over it.
func TestFailoverNeverOverwritesUnreadablePITRState(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	r := newFailRig(t, false)
	o := r.n.orch
	inst, err := o.operated.SetPITR(ctx, r.inst, 7)
	must(t, err)
	must(t, o.store.Settings.Set(ctx, pitrKey(inst.ID), "{not json"))
	if err := o.pitrHold(ctx, r.tile(), inst, "x"); err == nil {
		t.Error("pitrHold wrote over an unreadable state")
	}
	if _, err := o.Switchover(ctx, r.inst.TileID, "w1"); err == nil {
		t.Error("the swap went through over an unreadable state")
	}
	if v, _, _ := o.store.Settings.Get(ctx, pitrKey(inst.ID)); v != "{not json" {
		t.Errorf("state row = %q, want it untouched", v)
	}
}
