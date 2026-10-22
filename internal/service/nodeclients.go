package service

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/FyrmForge/stackr/internal/clusterca"
	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/flow/move"
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
	controllerPrevKey  = installspec.ControllerPrevKeyFile
	controllerPrevCert = installspec.ControllerPrevCertFile
)

// revokedTTL is how long a revoked-or-not answer is reused: the TLS verify
// hook has no ctx and runs on every handshake.
const revokedTTL = 10 * time.Second

// clusterPKI is the controller's side of the cluster PKI: the CA, its own
// cert, and a client per worker node (Docker through the router, extras
// here).
type clusterPKI struct {
	ca *clusterca.CA
	// trust is the list every cert is checked against: the current root, and
	// the previous one while a rollover runs. nil = just ca.
	trust []*x509.Certificate
	// announced is the controller cert serials workers are told to accept
	// (hex): the current one, plus the next during a renewal. nil = no allow
	// list yet.
	announced []string
	// rollMu runs one controller cert renewal at a time.
	rollMu sync.Mutex
	// revoking is set while a RevokeControllerCert runs detached.
	revoking atomic.Bool
	own      tls.Certificate
	// prev is the cert own replaced in a renewal, kept until a revoke or
	// break glass (forgetPrev); it co-signs recovery answers. Empty = none.
	prev tls.Certificate
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
	// old: workers whose last probe showed a version this panel refuses new
	// Docker calls for (id -> that version, "" = none reported). In memory
	// only; the next probe fills it.
	old map[string]string
	// skew: how far each worker's clock is from the controller's, as its
	// last probe measured (positive = ahead). In memory only; a worker that
	// does not report its time, or did not answer, has none.
	skew map[string]time.Duration
}

// tooOldVersion is the version of a worker the gate refuses, and whether it does.
func (c *clusterPKI) tooOldVersion(id string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, bad := c.old[id]
	return v, bad
}

// roots is the live trust list (a clusterca.Roots). A rollover swaps it with
// setRoots; a cluster that never rolled over has one root, ca's.
func (c *clusterPKI) roots() []*x509.Certificate {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.trust) == 0 {
		return []*x509.Certificate{c.ca.Cert}
	}
	return c.trust
}

// setRoots replaces the trust list.
func (c *clusterPKI) setRoots(roots []*x509.Certificate) {
	c.mu.Lock()
	c.trust = roots
	c.mu.Unlock()
}

// serials is a copy of the announced controller serials.
func (c *clusterPKI) serials() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.announced)
}

// setSerials replaces the announced controller serials; the next mesh sync
// carries them to the workers.
func (c *clusterPKI) setSerials(serials []string) {
	c.mu.Lock()
	c.announced = slices.Clone(serials)
	c.mu.Unlock()
}

// meshTrust is what every worker's mesh spec carries (meshSvc.Trust): the
// trust list as PEM and the announced controller serials. nil without a
// cluster (single node).
func (o *Orchestrator) meshTrust(context.Context) (roots, controllerSerials []string) {
	if o.cluster == nil {
		return nil, nil
	}
	return clusterca.RootsToPEM(o.cluster.roots()), o.cluster.serials()
}

// cert is the controller's current cert (renew swaps it).
func (c *clusterPKI) cert() tls.Certificate {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.own
}

// prevCert is the controller's previous cert; empty when there is none.
func (c *clusterPKI) prevCert() tls.Certificate {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.prev
}

// keepPrev makes the controller's current cert files the previous ones,
// before a renewal overwrites them (rollController, not a revoke).
func keepPrev(dataDir string) error {
	// Both written aside first, then renamed: a crash leaves at most a torn
	// pair across the two renames, not a half-written file.
	pairs := [][2]string{{controllerKeyFile, controllerPrevKey}, {controllerCertFile, controllerPrevCert}}
	for _, f := range pairs {
		b, err := os.ReadFile(filepath.Join(dataDir, f[0]))
		if err != nil {
			return err
		}
		if err := os.WriteFile(filepath.Join(dataDir, f[1]+".new"), b, 0o600); err != nil {
			return err
		}
	}
	for _, f := range pairs {
		if err := os.Rename(filepath.Join(dataDir, f[1]+".new"), filepath.Join(dataDir, f[1])); err != nil {
			return err
		}
	}
	return nil
}

// forgetPrev drops the previous controller cert, from memory first (no
// answer is co-signed from here), then its files. Revoke and break glass.
func (c *clusterPKI) forgetPrev(dataDir string) error {
	c.mu.Lock()
	c.prev = tls.Certificate{}
	c.mu.Unlock()
	for _, f := range []string{controllerPrevKey, controllerPrevCert} {
		if err := os.Remove(filepath.Join(dataDir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// loadPrev reads the previous controller cert; none, or the current one (a
// crash between keepPrev and the switch), is no previous cert.
func loadPrev(dataDir string, own tls.Certificate) tls.Certificate {
	pair, err := tls.LoadX509KeyPair(filepath.Join(dataDir, controllerPrevCert), filepath.Join(dataDir, controllerPrevKey))
	if err != nil || len(own.Certificate) == 0 || bytes.Equal(pair.Certificate[0], own.Certificate[0]) {
		return tls.Certificate{}
	}
	return pair
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
	roots, err := clusterca.TrustRoots(dataDir)
	if err != nil {
		return nil, fmt.Errorf("cluster CA: %w", err)
	}
	// The list holds the current serial from the first boot, so workers
	// enforce it (an empty list means accept any, ca-rollover.md C1).
	serial, err := certSerial(own)
	if err != nil {
		return nil, fmt.Errorf("controller cert: %w", err)
	}
	return &clusterPKI{ca: ca, trust: roots, announced: []string{serial}, own: own, prev: loadPrev(dataDir, own), st: st, rev: map[string]revAns{},
		addr: map[string]string{}, ext: map[string]*workerclient.Client{},
		probe: map[string]probeEnt{}, fails: map[string]int{}, alive: map[string]bool{}, old: map[string]string{}, skew: map[string]time.Duration{}}, nil
}

// controllerCert loads the controller's cert and key, or issues a new pair.
// A cert in its last third is kept: renewal goes through the announce
// (rollController), so workers learn the next serial before it is used.
func controllerCert(ctx context.Context, st *store.Store, dataDir string, ca *clusterca.CA) (tls.Certificate, error) {
	kp, cp := filepath.Join(dataDir, controllerKeyFile), filepath.Join(dataDir, controllerCertFile)
	if pair, err := tls.LoadX509KeyPair(cp, kp); err == nil {
		if c, err := clusterca.ParseCertPEM(mustRead(cp)); err == nil &&
			time.Now().Before(c.NotAfter) && c.CheckSignatureFrom(ca.Cert) == nil {
			return pair, nil
		}
	}
	n, err := issueController(ctx, st, ca)
	if err != nil {
		return tls.Certificate{}, err
	}
	return n.pair, n.save(dataDir)
}

// newController is a controller cert issued but not yet in use.
type newController struct {
	pair            tls.Certificate
	certPEM, keyPEM []byte
	serial          string
}

// issueController signs a controller cert and records and audits it. Nothing
// is written to disk: save does that once the cert is to be used, so a crash
// in between leaves only an unused row.
func issueController(ctx context.Context, st *store.Store, ca *clusterca.CA) (newController, error) {
	key, err := clusterca.NewKey()
	if err != nil {
		return newController{}, err
	}
	csr, err := clusterca.NewCSR(key)
	if err != nil {
		return newController{}, err
	}
	iss, err := ca.IssueController(csr, time.Now())
	if err != nil {
		return newController{}, err
	}
	err = st.Tx(ctx, func(tx store.Tx) error {
		if err := tx.NodeCerts.Create(ctx, store.NodeCert{
			Serial: iss.Serial, Role: iss.Role, Issuer: iss.Issuer, NotAfter: iss.NotAfter, CreatedAt: time.Now().UTC(),
		}); err != nil {
			return err
		}
		return audit(tx).Append(ctx, securitylog.Event{
			Action: securitylog.CertIssue, TargetKind: "cert", TargetID: iss.Serial, Detail: iss.Role,
		})
	})
	if err != nil {
		return newController{}, err
	}
	kb, err := clusterca.KeyPEM(key)
	if err != nil {
		return newController{}, err
	}
	pair, err := tls.X509KeyPair(iss.CertPEM, kb)
	return newController{pair, iss.CertPEM, kb, iss.Serial}, err
}

// save writes the pair to the data dir (both files are panel state). Both
// are written aside first, then renamed, as keepPrev does: no reader sees a
// half-written file, and the panel backup holds rollMu over the gap between
// the two renames.
func (n newController) save(dataDir string) error {
	kp, cp := filepath.Join(dataDir, controllerKeyFile), filepath.Join(dataDir, controllerCertFile)
	if err := os.MkdirAll(filepath.Dir(kp), 0o700); err != nil {
		return err
	}
	files := []struct {
		path string
		b    []byte
		mode os.FileMode
	}{{kp, n.keyPEM, 0o600}, {cp, n.certPEM, 0o644}}
	for _, f := range files {
		if err := os.WriteFile(f.path+".new", f.b, f.mode); err != nil {
			return err
		}
	}
	for _, f := range files {
		if err := os.Rename(f.path+".new", f.path); err != nil {
			return err
		}
	}
	return nil
}

// certSerial is the hex serial workers match against ControllerSerials (the
// same form as clusterca.Peer.Serial).
func certSerial(c tls.Certificate) (string, error) {
	if len(c.Certificate) == 0 {
		return "", errors.New("no certificate")
	}
	x, err := x509.ParseCertificate(c.Certificate[0])
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(x.SerialNumber.Bytes()), nil
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
	ext := workerclient.New(addr, clusterca.ClientConfig(c.roots, c.own, id, c.revoked))
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
		ver  string // the version its facts reported
		f    worker.Facts
		skew time.Duration
		// skewOK: the worker reported its time, so skew is known
		skewOK bool
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
			sent := time.Now()
			f, err := cd.ext.Facts(pctx)
			cd.up, cd.ver, cd.f = err == nil, f.Version, f
			if cd.up {
				cd.skew, cd.skewOK = clockSkew(sent, time.Now(), f.TimeMS)
			}
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
			delete(c.old, id)
			delete(c.skew, id)
		}
	}
	c.mu.Unlock()

	want := map[string]string{}
	var failed []error
	changed := false
	for _, cd := range cands {
		n := cd.n
		allowed := !cd.up || o.versionAllowed(ctx, cd.ver)
		c.mu.Lock()
		if cd.skewOK {
			c.skew[n.ID] = cd.skew
		} else {
			delete(c.skew, n.ID)
		}
		if cd.up {
			c.fails[n.ID] = 0
			if allowed {
				delete(c.old, n.ID)
			} else {
				c.old[n.ID] = cd.ver
			}
		} else {
			c.fails[n.ID]++
		}
		fails := c.fails[n.ID]
		c.mu.Unlock()
		status, prev := n.Status, n.Status
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
			if a, ok := nodeEvent(n.Name, prev, status); ok {
				o.alert(a)
			}
			if status == node.StatusOffline {
				o.markSeen(ctx, n.ID)
			}
			if status == node.StatusOnline {
				o.workerBack(ctx, n, &cd.f)
			}
			changed = true
		}
		if status != node.StatusOnline {
			continue
		}
		if cd.up {
			o.diskAlert(ctx, n, cd.f.DiskFree, cd.f.DiskTotal)
			o.clockAlert(ctx, n, cd.skew, cd.skewOK)
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
		d.SetTool(move.RsyncImage) // loaded from the registry, see EnsureTool
		d.SetClusterSource(o.clusterImage)
		r.Set(n.ID, d)
		if nv, ok := o.vip.(*nodeVIPs); ok {
			vctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			if !cd.up {
				// It just failed the probe: asking again would hold up boot
				// 5 s per worker; the jump check clears any doubled jumps.
				cancel()
			}
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
			o.controllerDiskAlert(context.Background())
			if _, err := o.SweepPendingConnectors(context.Background(), time.Now()); err != nil {
				slog.Warn("connectors: pending sweep failed", "err", err)
			}
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
	return nil, o.nodeDown(context.Background(), nodeID)
}

// clockSkew is how far a worker's clock (ms, unix milliseconds it put in
// its facts) is from ours, taken at the midpoint of the request: sent before
// it, got after. ms 0 is an older worker that does not say: unknown.
func clockSkew(sent, got time.Time, ms int64) (time.Duration, bool) {
	if ms == 0 {
		return 0, false
	}
	return time.UnixMilli(ms).Sub(sent.Add(got.Sub(sent) / 2)), true
}

// skewOf is a worker's last measured clock skew; false = not known.
func (o *Orchestrator) skewOf(nodeID string) (time.Duration, bool) {
	if o.cluster == nil {
		return 0, false
	}
	o.cluster.mu.Lock()
	defer o.cluster.mu.Unlock()
	d, ok := o.cluster.skew[nodeID]
	return d, ok
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
