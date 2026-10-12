package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/vip"
	"github.com/FyrmForge/stackr/internal/service/internal/workerclient"
	"github.com/FyrmForge/stackr/internal/worker"
)

// The last wiring of the multi-node build: the mesh service, the controller's
// own node in it, the VIP table of a worker, and the daily chores.

// startMesh builds the mesh service and runs it until Close. The controller's
// own node is in the mesh when Config.MeshWG is set (a real host); a fake
// host or a test leaves it out.
func (o *Orchestrator) startMesh(ctlID string) {
	var local *worker.Server
	if o.cfg.MeshWG != nil {
		runner := o.cfg.MeshRunner
		if runner == nil {
			runner = worker.ExecRunner{}
		}
		// keys/wg.key is the panel's WireGuard identity (installspec.WGKeyFile).
		local = worker.New(worker.Config{
			DockerSock: "/var/run/docker.sock",
			StateDir:   filepath.Join(o.cfg.DataDir, "keys"),
			Runner:     runner,
			Mesh:       worker.MeshConfig{WG: o.cfg.MeshWG, WGPort: o.cfg.MeshWGPort},
		})
	}
	o.mesh = newMeshSvc(o.store, func(id string) (MeshNode, bool) {
		if id == ctlID {
			// A single-node install runs no mesh interface.
			if ns, err := o.store.Nodes.List(context.Background()); local == nil || err != nil || len(ns) < 2 {
				return nil, false
			}
			return localMesh{local}, true
		}
		c, err := o.nodeExtras(id)
		if err != nil {
			return nil, false
		}
		return c, true
	})
	ctx, cancel := context.WithCancel(context.Background())
	o.stopMesh = cancel
	if local != nil {
		o.mesh.ctlID, o.mesh.Registry, o.mesh.Panel = ctlID, o.meshRegistry, o.panelURL()
	}
	// Not started here: its first sync reads leaves New builds after this.
	o.runMesh = func() { go o.mesh.Run(ctx) }
	if local != nil {
		go o.serveRegistryWG(ctx)
		go func() {
			defer local.Stop()
			t := time.NewTicker(30 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if err := local.Reassert(ctx); err != nil && ctx.Err() == nil {
						slog.Warn("mesh: controller exit rules", "err", err)
					}
					if err := o.sync.Retry(ctx); err != nil && ctx.Err() == nil {
						slog.Warn("proxy: retry", "err", err)
					}
				}
			}
		}()
	}
}

// kickMesh asks for a mesh sync soon.
func (o *Orchestrator) kickMesh() {
	if o.mesh != nil {
		o.mesh.Kick()
	}
}

// ensureMesh pushes the mesh to the node in ctx before containers start on
// it, so their traffic never leaves by the wrong uplink. A single-node
// install has nothing to push. A failure is logged: a node that cannot be
// reached fails the deploy on its own.
func (o *Orchestrator) ensureMesh(ctx context.Context) {
	if o.mesh == nil {
		return
	}
	id, ok := docker.NodeFrom(ctx)
	if !ok {
		return
	}
	if ns, err := o.store.Nodes.List(ctx); err != nil || len(ns) < 2 {
		return
	}
	if err := o.mesh.Ensure(ctx, id); err != nil {
		slog.Warn("mesh: ensure before start", "node", id, "err", err)
	}
}

// localMesh is the controller's own node as a MeshNode: the same worker code
// a remote node runs, called in process.
type localMesh struct{ s *worker.Server }

func (l localMesh) Facts(ctx context.Context) (worker.Facts, error) {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/facts", nil).WithContext(ctx)
	l.s.Handler().ServeHTTP(rec, req)
	var f worker.Facts
	if rec.Code != 200 {
		return f, fmt.Errorf("facts: %d", rec.Code)
	}
	return f, json.Unmarshal(rec.Body.Bytes(), &f)
}

func (l localMesh) MeshInfo(context.Context) (worker.MeshInfo, error) { return l.s.MeshInfo() }

func (l localMesh) ApplyMesh(ctx context.Context, spec worker.MeshSpec) error {
	return l.s.ApplyMesh(ctx, spec)
}

// workerVIP is a worker's VIP table: the vip.Table logic, with its
// iptables-restore sent through the worker's extras API and the jumps (which
// the table installs with `iptables -C/-I`, argv the worker does not take)
// sent as restore payloads. After every change the whole table is saved as the
// worker's state, so a reboot replays it.
//
// The jumps are tracked here, not read back from the node; the worker keeps
// them at rule 1 itself (after each push, at replay and every 30 s), since
// Docker puts its own rules on top when it makes a network or restarts.
type workerVIP struct {
	*vip.Table
	ext *workerclient.Client

	op   sync.Mutex // one table change at a time
	jm   sync.Mutex // guards ins and pend, which the table's hooks touch
	ins  map[string]string
	pend []string // "table/hook" jumps added by the change in flight
}

func newWorkerVIP(ctx context.Context, ext *workerclient.Client) *workerVIP {
	w := &workerVIP{Table: vip.New(), ext: ext, ins: map[string]string{}}
	w.Restore = func(ctx context.Context, script string) error { return ext.ApplyIPTables(ctx, script) }
	w.Exec, w.Read = w.exec, w.read
	// A controller restart must not insert the jumps a second time.
	if st, err := ext.GetState(ctx); err == nil {
		w.seed(st.Jumps)
	}
	return w
}

var errNotEmulated = errors.New("not emulated")

// iptArgs splits an iptables argv into its table and the rest after it.
func iptArgs(argv []string) (tbl string, rest []string) {
	rest = argv[1:]
	tbl = "filter"
	if len(rest) > 2 && rest[0] == "-t" {
		tbl, rest = rest[1], rest[2:]
	}
	return
}

func (w *workerVIP) exec(_ context.Context, argv ...string) error {
	tbl, rest := iptArgs(argv)
	w.jm.Lock()
	defer w.jm.Unlock()
	switch {
	case len(rest) >= 4 && rest[0] == "-C": // -C hook -j chain: present if we put it there
		if w.ins[tbl+"/"+rest[1]] == rest[3] {
			return nil
		}
	case len(rest) >= 4 && rest[0] == "-I": // -I hook [1] -j chain
		key := tbl + "/" + rest[1]
		w.ins[key] = rest[len(rest)-1]
		w.pend = append(w.pend, key)
		return nil
	case len(rest) >= 2 && rest[0] == "-S": // DOCKER-USER exists on any Docker 17+ host
		return nil
	}
	return errNotEmulated // also ends the table's `-D` loops
}

func (w *workerVIP) read(_ context.Context, argv ...string) (string, error) {
	tbl, rest := iptArgs(argv)
	w.jm.Lock()
	defer w.jm.Unlock()
	if len(rest) >= 2 && rest[0] == "-S" {
		if c := w.ins[tbl+"/"+rest[1]]; c != "" {
			return "-A " + rest[1] + " -j " + c + "\n", nil
		}
	}
	return "", errNotEmulated
}

// jumps is the restore payload of every jump we hold.
func (w *workerVIP) jumps() string {
	w.jm.Lock()
	defer w.jm.Unlock()
	return jumpPayload(w.ins, nil)
}

// jumpPayload renders the jumps in ins (only those in keys when given).
func jumpPayload(ins map[string]string, keys []string) string {
	by := map[string][]string{}
	for k, chain := range ins {
		if keys != nil && !slices.Contains(keys, k) {
			continue
		}
		tbl, hook, _ := strings.Cut(k, "/")
		by[tbl] = append(by[tbl], fmt.Sprintf("-I %s 1 -j %s\n", hook, chain))
	}
	var b strings.Builder
	for _, tbl := range slices.Sorted(maps.Keys(by)) {
		b.WriteString("*" + tbl + "\n")
		slices.Sort(by[tbl])
		for _, l := range by[tbl] {
			b.WriteString(l)
		}
		b.WriteString("COMMIT\n")
	}
	return b.String()
}

// seed reads back a jumps payload this type wrote.
func (w *workerVIP) seed(payload string) {
	tbl := ""
	for _, l := range strings.Split(payload, "\n") {
		f := strings.Fields(l)
		switch {
		case len(f) == 1 && strings.HasPrefix(f[0], "*"):
			tbl = f[0][1:]
		case len(f) == 5 && f[0] == "-I" && tbl != "":
			w.ins[tbl+"/"+f[1]] = f[4]
		}
	}
}

// finish runs after a table change: the jumps it added are sent and the whole
// table saved. A change that failed forgets its jumps, so the next one adds
// them again.
func (w *workerVIP) finish(ctx context.Context, err error) error {
	w.jm.Lock()
	pend := w.pend
	w.pend = nil
	w.jm.Unlock()
	forget := func() {
		w.jm.Lock()
		for _, k := range pend {
			delete(w.ins, k)
		}
		w.jm.Unlock()
	}
	if err != nil {
		forget()
		return err
	}
	if len(pend) > 0 {
		w.jm.Lock()
		payload := jumpPayload(w.ins, pend)
		w.jm.Unlock()
		if err := w.ext.ApplyIPTables(ctx, payload); err != nil {
			forget()
			return err
		}
	}
	return w.save(ctx)
}

// save writes the table and its jumps as the worker's saved state, leaving
// the other parts (wg, routes) alone.
func (w *workerVIP) save(ctx context.Context) error {
	st, err := w.ext.GetState(ctx)
	if err != nil && !strings.Contains(err.Error(), "404") {
		return err
	}
	st.Chains, st.Jumps = w.Full(), w.jumps()
	return w.ext.PutState(ctx, st)
}

func (w *workerVIP) Set(ctx context.Context, v string, replicas, lan []string) error {
	w.op.Lock()
	defer w.op.Unlock()
	return w.finish(ctx, w.Table.Set(ctx, v, replicas, lan))
}

func (w *workerVIP) Remove(ctx context.Context, v string) error {
	w.op.Lock()
	defer w.op.Unlock()
	return w.finish(ctx, w.Table.Remove(ctx, v))
}

func (w *workerVIP) Allow(ctx context.Context, ip string, lan []string) error {
	w.op.Lock()
	defer w.op.Unlock()
	return w.finish(ctx, w.Table.Allow(ctx, ip, lan))
}

func (w *workerVIP) Rebuild(ctx context.Context, all map[string]vip.Entry, b vip.Base) error {
	w.op.Lock()
	defer w.op.Unlock()
	return w.finish(ctx, w.Table.Rebuild(ctx, all, b))
}

func (w *workerVIP) Merge(ctx context.Context, all map[string]vip.Entry, drop []string, b vip.Base) error {
	w.op.Lock()
	defer w.op.Unlock()
	return w.finish(ctx, w.Table.Merge(ctx, all, drop, b))
}

// AllowTCP is the live move's rsync door on this node (the worker holds the
// INPUT and the forward rule, see worker.AllowTCP).
func (w *workerVIP) AllowTCP(ctx context.Context, src string, lo, hi int, on bool) error {
	return w.ext.AllowTCP(ctx, src, lo, hi, on)
}

// registryTokenKeep is how long an expired registry token row stays, as the
// audit trail of who pulled what.
const registryTokenKeep = 7 * 24 * time.Hour

// dailyChores renews the controller cert when it is due and prunes expired
// registry tokens.
func (o *Orchestrator) dailyChores(ctx context.Context) {
	if o.cluster != nil {
		changed, err := o.cluster.renew(ctx, o.cfg.DataDir)
		if err != nil {
			slog.Warn("cluster: controller cert renewal failed", "err", err)
		} else if changed {
			slog.Info("cluster: controller cert renewed")
		}
	}
	if err := o.store.RegistryTokens.DeleteExpiredBefore(ctx, time.Now().UTC().Add(-registryTokenKeep)); err != nil {
		slog.Warn("registry: prune expired tokens", "err", err)
	}
}
