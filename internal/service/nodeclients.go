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
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/internal/workerclient"
)

// WorkerPort is where `stackrd worker` listens on the node's WireGuard
// address. A node's wg_address may carry its own ":port" (tests, odd setups).
const WorkerPort = "7443"

// Controller cert files under the data dir. They are not in the panel state
// set: a restored panel has the CA and issues a new controller cert.
const (
	controllerKeyFile  = "keys/controller.key"
	controllerCertFile = "keys/controller.crt"
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
// renewal is checked at boot only; a panel up for two thirds of 90 days
// needs a periodic check.
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
		addr: map[string]string{}, ext: map[string]*workerclient.Client{}}, nil
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

// refreshNodeClients makes the router and the extras clients match the
// nodes table: an online worker with an address gets a Docker client (mTLS,
// pinned to its node id) and an extras client; a node that is gone, not
// online or moved loses them. Clients it did not make (the controller's, a
// test's) are never touched. Called at boot, by every verb that changes
// nodes, and on a timer.
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
	want := map[string]string{}
	var failed []error
	for _, n := range ns {
		if n.Role != node.Worker || n.Status != node.StatusOnline {
			continue
		}
		addr := nodeAddr(n)
		if addr == "" {
			continue
		}
		want[n.ID] = addr
		c.mu.Lock()
		same := c.addr[n.ID] == addr
		c.mu.Unlock()
		if same {
			continue
		}
		ext := workerclient.New(addr, clusterca.ClientConfig(c.ca.Cert, c.own, n.ID, c.revoked))
		opts := ext.DockerOptions()
		d, err := docker.NewRemote(opts.Host, opts.HTTPClient)
		if err != nil {
			failed = append(failed, fmt.Errorf("node %s: %w", n.Name, err))
			continue
		}
		r.Set(n.ID, d)
		c.mu.Lock()
		c.addr[n.ID], c.ext[n.ID] = addr, ext
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
	}
	return errors.Join(failed...)
}

// watchNodeClients keeps the clients fresh until Close.
func (o *Orchestrator) watchNodeClients() {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-o.stopNodes:
			return
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
