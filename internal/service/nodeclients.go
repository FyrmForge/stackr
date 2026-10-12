package service

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/clusterca"
	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/internal/workerclient"
	"github.com/FyrmForge/stackr/internal/worker"
)

// WorkerPort is where `stackrd worker` listens on the node's WireGuard
// address. A node's wg_address may carry its own ":port" (tests, odd setups).
const WorkerPort = "7443"

// Controller cert files under the data dir; both are in the panel state set.
const (
	controllerKeyFile  = installspec.ControllerKeyFile
	controllerCertFile = installspec.ControllerCertFile
)

// revokedTTL is how long a revoked-or-not answer is reused: the TLS verify
// hook has no ctx and runs on every handshake.
const revokedTTL = 10 * time.Second

// clusterPKI is the controller's side of the cluster PKI: the CA, its own
// cert, and a client per worker node (Docker through the router, extras
// here).
type clusterPKI struct {
	ca   *clusterca.CA
	own  tls.Certificate
	st   *store.Store
	mu   sync.Mutex
	rev  map[string]revAns
	addr map[string]string // node id -> address its clients were built for
	ext  map[string]*workerclient.Client
	// probe is the client of every worker that could be online, fails the
	// refreshes each missed in a row.
	probe map[string]probeEnt
	fails map[string]int
	// alive: workers that miss the tunnel but answer their public liveness
	// port ("tunnel down, host alive"). In memory only.
	alive map[string]bool
}

// cert is the controller's current cert (renew swaps it).
func (c *clusterPKI) cert() tls.Certificate {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.own
}

type revAns struct {
	revoked bool
	at      time.Time
}

// revoked is the serial check handed to clusterca. It fails closed: a cert
// whose status cannot be read is refused.
func (c *clusterPKI) revoked(serial string) bool {
	c.mu.Lock()
	if a, ok := c.rev[serial]; ok && time.Since(a.at) < revokedTTL {
		c.mu.Unlock()
		return a.revoked
	}
	c.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	r, err := c.st.NodeCerts.IsRevoked(ctx, serial)
	if err != nil {
		slog.Warn("cluster: revoked check failed, refusing the cert", "serial", serial, "err", err)
		return true
	}
	c.mu.Lock()
	c.rev[serial] = revAns{r, time.Now()}
	c.mu.Unlock()
	return r
}

// bootCluster makes the cluster CA and the controller cert when missing (or
// when the cert is due for renewal) and audits each cert it issues. ponytail:
// renewal is checked here at boot and daily after (renew).
// renew replaces the controller cert when it is due (controllerCert decides)
// and makes the clients dial with the new one. It reports whether it did.
func (c *clusterPKI) renew(ctx context.Context, dataDir string) (bool, error) {
	own, err := controllerCert(ctx, c.st, dataDir, c.ca)
	if err != nil {
		return false, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(own.Certificate) > 0 && len(c.own.Certificate) > 0 && string(own.Certificate[0]) == string(c.own.Certificate[0]) {
		return false, nil
	}
	c.own = own
	// Clients are rebuilt with the new cert on the next refresh; the router
	// keeps serving through the old ones until then.
	clear(c.addr)
	clear(c.ext)
	clear(c.probe)
	return true, nil
}

func bootCluster(ctx context.Context, st *store.Store, dataDir string) (*clusterPKI, error) {
	ca, err := clusterca.LoadCA(dataDir)
	if errors.Is(err, os.ErrNotExist) {
		if ca, err = clusterca.NewCA(time.Now()); err == nil {
			err = clusterca.SaveCA(dataDir, ca)
		}
	}
	if err != nil {
		return nil, fmt.Errorf("cluster CA: %w", err)
	}
	own, err := controllerCert(ctx, st, dataDir, ca)
	if err != nil {
		return nil, fmt.Errorf("controller cert: %w", err)
	}
	return &clusterPKI{ca: ca, own: own, st: st, rev: map[string]revAns{},
		addr: map[string]string{}, ext: map[string]*workerclient.Client{},
		probe: map[string]probeEnt{}, fails: map[string]int{}, alive: map[string]bool{}}, nil
}

// controllerCert loads the controller's cert and key, or issues a new pair.
func controllerCert(ctx context.Context, st *store.Store, dataDir string, ca *clusterca.CA) (tls.Certificate, error) {
	kp, cp := filepath.Join(dataDir, controllerKeyFile), filepath.Join(dataDir, controllerCertFile)
	if pair, err := tls.LoadX509KeyPair(cp, kp); err == nil {
		if c, err := clusterca.ParseCertPEM(mustRead(cp)); err == nil &&
			!clusterca.NeedsRenewal(c, time.Now()) && c.CheckSignatureFrom(ca.Cert) == nil {
			return pair, nil
		}
	}
	key, err := clusterca.NewKey()
	if err != nil {
		return tls.Certificate{}, err
	}
	csr, err := clusterca.NewCSR(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	iss, err := ca.IssueController(csr, time.Now())
	if err != nil {
		return tls.Certificate{}, err
	}
	// Row and audit event commit together; the files follow, and a crash
	// between them only leaves an unused row.
	err = st.Tx(ctx, func(tx store.Tx) error {
		if err := tx.NodeCerts.Create(ctx, store.NodeCert{
			Serial: iss.Serial, Role: iss.Role, NotAfter: iss.NotAfter, CreatedAt: time.Now().UTC(),
		}); err != nil {
			return err
		}
		return audit(tx).Append(ctx, securitylog.Event{
			Action: securitylog.CertIssue, TargetKind: "cert", TargetID: iss.Serial, Detail: iss.Role,
		})
	})
	if err != nil {
		return tls.Certificate{}, err
	}
	kb, err := clusterca.KeyPEM(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	if err := os.MkdirAll(filepath.Dir(kp), 0o700); err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(kp, kb, 0o600); err != nil {
		return tls.Certificate{}, err
	}
	if err := os.WriteFile(cp, iss.CertPEM, 0o644); err != nil {
		return tls.Certificate{}, err
	}
	return tls.X509KeyPair(iss.CertPEM, kb)
}

func mustRead(p string) []byte { b, _ := os.ReadFile(p); return b }

// nodeAddr is where a worker node's listener is: its wg address, without a
// prefix length, on WorkerPort unless the address names a port.
func nodeAddr(n store.Node) string {
	a, _, _ := strings.Cut(strings.TrimSpace(n.WGAddress), "/")
	if a == "" {
		return ""
	}
	if _, _, err := net.SplitHostPort(a); err == nil {
		return a
	}
	return net.JoinHostPort(a, WorkerPort)
}

// failsBeforeOffline is how many refreshes in a row a worker must miss
// before it is marked offline and loses its router entry.
const failsBeforeOffline = 2

// probeEnt is the extras client a node is probed with, kept so its idle
// connections are reused.
type probeEnt struct {
	addr string
	ext  *workerclient.Client
}

// probeClient is the node's extras client for addr: the live one, or a
// cached one for a node that is not online yet.
func (c *clusterPKI) probeClient(id, addr string) *workerclient.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.addr[id] == addr && c.ext[id] != nil {
		return c.ext[id]
	}
	if e, ok := c.probe[id]; ok && e.addr == addr {
		return e.ext
	}
	ext := workerclient.New(addr, clusterca.ClientConfig(c.ca.Cert, c.own, id, c.revoked))
	c.probe[id] = probeEnt{addr, ext}
	return ext
}

// refreshNodeClients makes the router, the extras clients and the VIP tables
// match the nodes table, and keeps each worker's status true: a confirmed
// worker (joined) or an offline one that answers its extras API becomes
// online; an online one that misses failsBeforeOffline refreshes in a row
// becomes offline. An online worker gets a Docker client (mTLS, pinned to its
// node id), an extras client and a VIP table; one that is gone, not online or
// moved loses them. Pending nodes are never touched. Clients it did not make
// (the controller's, a test's) are left alone. Called at boot, by every verb
// that changes nodes, and on a timer.
func (o *Orchestrator) refreshNodeClients(ctx context.Context) error {
	r, ok := o.docker.(*DockerRouter)
	if !ok || o.cluster == nil {
		return nil
	}
	ns, err := node.New(o.store.Nodes).List(ctx)
	if err != nil {
		return err
	}
	c := o.cluster
	type cand struct {
		n    store.Node
		addr string
		ext  *workerclient.Client
		up   bool
	}
	var cands []*cand
	seen := map[string]bool{}
	for _, n := range ns {
		if n.Role != node.Worker {
			continue
		}
		switch n.Status {
		case node.StatusOnline, node.StatusOffline, node.StatusJoined:
		default: // pending: no range, no cert yet
			continue
		}
		// Only through the tunnel: a new worker brings it up from the mesh
		// spec in its join answer.
		addr := nodeAddr(n)
		if addr == "" {
			continue
		}
		seen[n.ID] = true
		cands = append(cands, &cand{n: n, addr: addr, ext: c.probeClient(n.ID, addr)})
	}
	var wg sync.WaitGroup
	for _, cd := range cands {
		wg.Add(1)
		go func() {
			defer wg.Done()
			pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			_, err := cd.ext.Facts(pctx)
			cd.up = err == nil
			alive := false
			if !cd.up && o.mesh != nil {
				// Its own deadline: a dead tunnel spends all of pctx on Facts.
				actx, acancel := context.WithTimeout(ctx, 5*time.Second)
				defer acancel()
				for _, a := range o.mesh.publicOf(cd.n.ID) {
					if alive = worker.Alive(actx, a.String()); alive {
						break
					}
				}
			}
			c.mu.Lock()
			c.alive[cd.n.ID] = alive
			c.mu.Unlock()
		}()
	}
	wg.Wait()
	c.mu.Lock()
	for id := range c.probe {
		if !seen[id] {
			delete(c.probe, id)
			delete(c.fails, id)
			delete(c.alive, id)
		}
	}
	c.mu.Unlock()

	want := map[string]string{}
	var failed []error
	changed := false
	for _, cd := range cands {
		n := cd.n
		c.mu.Lock()
		if cd.up {
			c.fails[n.ID] = 0
		} else {
			c.fails[n.ID]++
		}
		fails := c.fails[n.ID]
		c.mu.Unlock()
		status := n.Status
		switch {
		case cd.up && status != node.StatusOnline:
			status = node.StatusOnline
		case !cd.up && status == node.StatusOnline && fails >= failsBeforeOffline:
			status = node.StatusOffline
		}
		if status != n.Status {
			n.Status = status
			if err := o.store.Nodes.Update(ctx, n); err != nil {
				failed = append(failed, fmt.Errorf("node %s: %w", n.Name, err))
				continue
			}
			slog.Info("nodes: status changed", "node", n.Name, "status", status)
			changed = true
		}
		if status != node.StatusOnline {
			continue
		}
		want[n.ID] = cd.addr
		c.mu.Lock()
		same := c.addr[n.ID] == cd.addr
		c.mu.Unlock()
		if same {
			continue
		}
		opts := cd.ext.DockerOptions()
		d, err := docker.NewRemote(opts.Host, opts.HTTPClient)
		if err != nil {
			failed = append(failed, fmt.Errorf("node %s: %w", n.Name, err))
			delete(want, n.ID)
			continue
		}
		r.Set(n.ID, d)
		if nv, ok := o.vip.(*nodeVIPs); ok {
			vctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			nv.Attach(n.ID, newWorkerVIP(vctx, cd.ext))
			cancel()
		}
		c.mu.Lock()
		c.addr[n.ID], c.ext[n.ID] = cd.addr, cd.ext
		c.mu.Unlock()
	}
	c.mu.Lock()
	var gone []string
	for id := range c.addr {
		if _, ok := want[id]; !ok {
			gone = append(gone, id)
			delete(c.addr, id)
			delete(c.ext, id)
		}
	}
	c.mu.Unlock()
	for _, id := range gone {
		r.Remove(id)
		if nv, ok := o.vip.(*nodeVIPs); ok {
			nv.Detach(id)
		}
	}
	if changed || len(gone) > 0 {
		o.kickMesh()
	}
	return errors.Join(failed...)
}

// watchNodeClients keeps the clients fresh until Close.
func (o *Orchestrator) watchNodeClients() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	day := time.NewTicker(24 * time.Hour)
	defer day.Stop()
	for {
		select {
		case <-o.stopNodes:
			return
		case <-day.C:
			o.dailyChores(context.Background())
		case <-t.C:
			if err := o.refreshNodeClients(context.Background()); err != nil {
				slog.Warn("nodes: client refresh failed", "err", err)
			}
		}
	}
}

// nodeExtras is the extras API client for a worker node (facts, iptables,
// wg, saved state, Caddy). The controller has none: it is local.
func (o *Orchestrator) nodeExtras(nodeID string) (*workerclient.Client, error) {
	if o.cluster != nil {
		o.cluster.mu.Lock()
		defer o.cluster.mu.Unlock()
		if c, ok := o.cluster.ext[nodeID]; ok {
			return c, nil
		}
	}
	return nil, fmt.Errorf("%w: no connection to node %s", errs.ErrBusy, nodeID)
}

// HostAlive reports a worker the tunnel cannot reach that still answers its
// public liveness port.
func (o *Orchestrator) HostAlive(nodeID string) bool {
	if o.cluster == nil {
		return false
	}
	o.cluster.mu.Lock()
	defer o.cluster.mu.Unlock()
	return o.cluster.alive[nodeID]
}
