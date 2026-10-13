package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

// recRunner is a worker's command runner over a tiny iptables: it records
// every restore payload and keeps the rules -I/-D/-C act on.
type recRunner struct {
	mu       sync.Mutex
	restores []string
	calls    []string
	rules    map[string]bool
}

func (r *recRunner) Run(_ context.Context, stdin []byte, name string, args ...string) ([]byte, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.rules == nil {
		r.rules = map[string]bool{}
	}
	r.calls = append(r.calls, name+" "+strings.Join(args, " "))
	if name == "iptables-restore" {
		r.restores = append(r.restores, string(stdin))
		return nil, nil
	}
	if name != "iptables" || len(args) < 2 {
		return nil, nil
	}
	switch args[0] {
	case "-I", "-C", "-D":
		rest := args[2:]
		if args[0] == "-I" {
			rest = args[3:] // -I chain 1 ...
		}
		key := args[1] + " " + strings.Join(rest, " ")
		switch args[0] {
		case "-I":
			r.rules[key] = true
		case "-C", "-D":
			if !r.rules[key] {
				return []byte("no such rule"), context.Canceled
			}
			if args[0] == "-D" {
				delete(r.rules, key)
			}
		}
	}
	return nil, nil
}

func (r *recRunner) count(sub string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, s := range r.restores {
		if strings.Contains(s, sub) {
			n++
		}
	}
	return n
}

func (r *recRunner) called(sub string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, c := range r.calls {
		if strings.Contains(c, sub) {
			return true
		}
	}
	return false
}

// A confirmed worker (joined) that answers becomes online and gets its
// clients; one that stops answering goes offline; a pending node is left
// alone throughout.
func TestWorkerStatusFollowsTheConnection(t *testing.T) {
	ctx := context.Background()
	o := newClusterPanel(t)
	_, srv, _ := workerNodeWith(t, o, "w1", node.StatusJoined, worker0())
	must(t, o.store.Nodes.Create(ctx, store.Node{
		ID: "p1", Name: "p1", Slug: "p1", Role: node.Worker, Tags: "[]", RangeCIDR: "pending:p1",
		WGAddress: srv.Listener.Addr().String() + "/32", Status: node.StatusPending, CreatedAt: time.Now(),
	}))
	status := func(id string) string {
		n, err := o.store.Nodes.Get(ctx, id)
		must(t, err)
		return n.Status
	}

	must(t, o.refreshNodeClients(ctx))
	if got := status("w1"); got != node.StatusOnline {
		t.Fatalf("joined worker that answers = %s", got)
	}
	if _, err := o.nodeExtras("w1"); err != nil {
		t.Errorf("no extras client once online: %v", err)
	}
	if got := status("p1"); got != node.StatusPending {
		t.Errorf("pending node = %s", got)
	}
	if _, err := o.nodeExtras("p1"); err == nil {
		t.Error("a pending node got a client")
	}

	srv.Close()
	must(t, o.refreshNodeClients(ctx))
	must(t, o.refreshNodeClients(ctx))
	if got := status("w1"); got != node.StatusOffline {
		t.Fatalf("worker that stopped answering = %s", got)
	}
	if _, err := o.vipFor(WithNode(ctx, "w1")); err == nil {
		t.Error("an offline node kept its VIP table")
	}
}

// The mesh never plans a pending node (its range is "pending:<id>").
func TestMeshSkipsPendingNodes(t *testing.T) {
	ctx := context.Background()
	o := newClusterPanel(t)
	must(t, o.store.Nodes.Create(ctx, store.Node{
		ID: "p1", Name: "p1", Slug: "p1", Role: node.Worker, Tags: "[]", RangeCIDR: "pending:p1",
		WGPublicKey: "k", Status: node.StatusPending, CreatedAt: time.Now(),
	}))
	in, err := o.mesh.gather(ctx)
	must(t, err)
	for _, n := range in.Nodes {
		if n.ID == "p1" {
			t.Fatal("gather returned the pending node")
		}
	}
}

// A worker's VIP table goes through its extras API: the chains, the jumps
// once, and the whole table saved as its state; a table built after a
// controller restart does not insert the jumps again.
func TestWorkerVIPTable(t *testing.T) {
	ctx := context.Background()
	o := newClusterPanel(t)
	rr := &recRunner{}
	workerNodeWith(t, o, "w1", node.StatusOnline, workerWith(rr, t.TempDir()))
	must(t, o.refreshNodeClients(ctx))

	wctx := WithNode(ctx, "w1")
	tbl, err := o.vipFor(wctx)
	must(t, err)
	must(t, tbl.Set(wctx, "10.208.1.2", []string{"10.208.1.3"}, nil))
	if rr.count("--to-destination 10.208.1.3") != 1 {
		t.Fatalf("the DNAT did not reach the worker: %q", rr.restores)
	}
	if rr.count("-I PREROUTING 1 -j STACKR-VIP") != 1 {
		t.Fatalf("jump sent %d times, want 1: %q", rr.count("-I PREROUTING 1 -j STACKR-VIP"), rr.restores)
	}
	must(t, tbl.Set(wctx, "10.208.1.2", []string{"10.208.1.4"}, nil))
	must(t, tbl.Remove(wctx, "10.208.9.9"))
	if rr.count("-I PREROUTING 1 -j STACKR-VIP") != 1 {
		t.Error("a later change sent the jump again")
	}

	ext, err := o.nodeExtras("w1")
	must(t, err)
	st, err := ext.GetState(ctx)
	must(t, err)
	if !strings.Contains(st.Chains, "--to-destination 10.208.1.4") || strings.Contains(st.Chains, "10.208.1.3") ||
		!strings.Contains(st.Jumps, "*nat") || !strings.Contains(st.Jumps, "-I PREROUTING 1 -j STACKR-VIP") {
		t.Fatalf("saved state = %+v", st)
	}

	again := newWorkerVIP(ctx, ext)
	must(t, again.Set(ctx, "10.208.2.2", []string{"10.208.2.3"}, nil))
	if rr.count("-I PREROUTING 1 -j STACKR-VIP") != 1 {
		t.Error("a table made after a restart inserted the jump again")
	}
}

// The live move's rsync door: INPUT and the forwarded (DNATed) path, both
// idempotent, closed again on off.
func TestWorkerVIPAllowTCP(t *testing.T) {
	ctx := context.Background()
	o := newClusterPanel(t)
	rr := &recRunner{}
	workerNodeWith(t, o, "w1", node.StatusOnline, workerWith(rr, t.TempDir()))
	must(t, o.refreshNodeClients(ctx))
	wctx := WithNode(ctx, "w1")
	tbl, err := o.vipFor(wctx)
	must(t, err)
	fw, ok := tbl.(interface {
		AllowTCP(context.Context, string, int, int, bool) error
	})
	if !ok {
		t.Fatal("a worker's table has no AllowTCP")
	}
	must(t, fw.AllowTCP(wctx, "10.207.255.1", 8873, 8874, true))
	must(t, fw.AllowTCP(wctx, "10.207.255.1", 8873, 8874, true))
	rr.mu.Lock()
	rules := len(rr.rules)
	has := func(sub string) bool {
		for k := range rr.rules {
			if strings.Contains(k, sub) {
				return true
			}
		}
		return false
	}
	ok = has("INPUT -s 10.207.255.1 -p tcp --dport 8873:8874") &&
		has("DOCKER-USER -s 10.207.255.1 -p tcp -m conntrack --ctorigdstport 8873:8874")
	rr.mu.Unlock()
	if rules != 2 || !ok {
		t.Fatalf("rules after two opens = %v", rr.rules)
	}
	must(t, fw.AllowTCP(wctx, "10.207.255.1", 8873, 8874, false))
	if len(rr.rules) != 0 {
		t.Errorf("rules after close = %v", rr.rules)
	}
	if err := fw.AllowTCP(wctx, "10.207.255.1; reboot", 1, 2, true); err == nil {
		t.Error("a bad source reached iptables")
	}
}

// A joining node gets the lowest free transit address, never the controller's.
func TestConfirmTakesTransit(t *testing.T) {
	w, adm, _ := joinWorld(t)
	ctx := context.Background()
	st, err := w.orch.RequestJoin(ctx, httpsMeta, joinReq(t, mint(t, w, adm, "w1").Token))
	must(t, err)
	n, err := w.orch.ConfirmJoin(ctx, adm, st.NodeID)
	must(t, err)
	if n.WGAddress != "10.207.255.1" {
		t.Errorf("transit = %s, want 10.207.255.1", n.WGAddress)
	}
}

// The daily chores renew a controller cert that is gone (the same path as one
// that is due) and prune registry token rows long expired.
func TestDailyChores(t *testing.T) {
	ctx := context.Background()
	o := newClusterPanel(t)
	now := time.Now().UTC()
	old, fresh := uuid.NewString(), uuid.NewString()
	for id, exp := range map[string]time.Time{old: now.Add(-30 * 24 * time.Hour), fresh: now.Add(-time.Minute)} {
		must(t, o.store.RegistryTokens.Create(ctx, store.RegistryToken{
			ID: id, Kind: "user", SubjectID: "u", Scope: "", CreatedAt: exp.Add(-time.Minute), ExpiresAt: exp,
		}))
	}
	before := o.cluster.own.Certificate[0]
	must(t, os.Remove(filepath.Join(o.cfg.DataDir, controllerCertFile)))
	o.dailyChores(ctx)

	if string(o.cluster.own.Certificate[0]) == string(before) {
		t.Error("the controller cert was not renewed")
	}
	es, err := o.SecurityLog(ctx, SecurityFilter{Action: securitylog.CertIssue})
	must(t, err)
	if len(es) != 2 {
		t.Errorf("cert.issue rows = %d, want the boot cert and the renewal", len(es))
	}
	if _, err := o.store.RegistryTokens.Get(ctx, old); err == nil {
		t.Error("a token expired a month ago was kept")
	}
	if _, err := o.store.RegistryTokens.Get(ctx, fresh); err != nil {
		t.Errorf("a token expired a minute ago was pruned: %v", err)
	}
}

func worker0() worker.Config { return worker.Config{} }

func workerWith(r worker.Runner, stateDir string) worker.Config {
	return worker.Config{Runner: r, StateDir: stateDir}
}
