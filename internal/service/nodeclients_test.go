package service

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/clusterca"
	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/dockerfake"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/environment"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/job"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/securitylog"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/worker"
)

// fakeDaemon is a Docker API on a unix socket that records what it is asked
// and answers just enough JSON.
type fakeDaemon struct {
	mu   sync.Mutex
	reqs []string
}

func (f *fakeDaemon) saw(sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.reqs {
		if strings.Contains(r, sub) {
			return true
		}
	}
	return false
}

func startFakeDaemon(t *testing.T) (*fakeDaemon, string) {
	t.Helper()
	f := &fakeDaemon{}
	sock := filepath.Join(t.TempDir(), "d.sock")
	ln, err := net.Listen("unix", sock)
	must(t, err)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.reqs = append(f.reqs, r.Method+" "+r.URL.Path)
		f.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Api-Version", "1.45")
		switch {
		case strings.HasSuffix(r.URL.Path, "/_ping"):
			_, _ = w.Write([]byte("OK"))
		case strings.HasSuffix(r.URL.Path, "/info"):
			_, _ = w.Write([]byte(`{"NCPU":6,"MemTotal":8589934592}`))
		case r.Method == "GET":
			_, _ = w.Write([]byte(`[]`))
		default:
			_, _ = w.Write([]byte(`{"Id":"net1"}`))
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return f, sock
}

// workerNode starts a worker (its Handler behind a TLS test server, cert from
// the panel's CA) over a fake daemon, and a node row pointing at it.
func workerNode(t *testing.T, o *Orchestrator, id string) (*fakeDaemon, *httptest.Server, clusterca.Issued) {
	t.Helper()
	return workerNodeWith(t, o, id, node.StatusOnline, worker.Config{})
}

// workerNodeWith is workerNode with the node row's status and the worker's
// config (its Docker socket is filled in).
func workerNodeWith(t *testing.T, o *Orchestrator, id, status string, cfg worker.Config) (*fakeDaemon, *httptest.Server, clusterca.Issued) {
	t.Helper()
	daemon, sock := startFakeDaemon(t)
	ca, err := clusterca.LoadCA(o.cfg.DataDir)
	must(t, err)
	key, err := clusterca.NewKey()
	must(t, err)
	csr, err := clusterca.NewCSR(key)
	must(t, err)
	iss, err := ca.IssueNode(csr, id, time.Now())
	must(t, err)
	kb, err := clusterca.KeyPEM(key)
	must(t, err)
	own, err := tls.X509KeyPair(iss.CertPEM, kb)
	must(t, err)

	cfg.DockerSock = sock
	if cfg.StateDir == "" { // the worker keeps its mesh keys here, not in the package directory
		cfg.StateDir = t.TempDir()
	}
	srv := httptest.NewUnstartedServer(worker.New(cfg).Handler())
	srv.TLS = clusterca.ServerConfig(ca.Cert, own, nil)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	must(t, o.store.Nodes.Create(context.Background(), store.Node{
		ID: id, Name: id, Slug: id, Role: node.Worker, Tags: "[]", RangeCIDR: "10." + strconv.Itoa(200+int(id[len(id)-1]-'0')) + ".0.0/16",
		WGAddress: srv.Listener.Addr().String() + "/32", Status: status, CreatedAt: time.Now(),
	}))
	return daemon, srv, iss
}

func newClusterPanel(t *testing.T) *Orchestrator {
	t.Helper()
	dir := t.TempDir()
	o, err := New(Config{DataDir: dir, SecretsKey: testKey, Conntrack: dir + "/nf_conntrack"},
		WithDocker(dockerfake.New()), WithVIP(vipStub{}),
		WithProxy(func(context.Context, json.RawMessage) error { return nil }))
	must(t, err)
	t.Cleanup(func() { _ = o.Close() })
	return o
}

// The CA and the controller cert are made on the first boot, once, audited;
// a second boot reuses both.
func TestBootMakesTheCAOnce(t *testing.T) {
	o := newClusterPanel(t)
	dir := o.cfg.DataDir
	for _, f := range []string{installspec.ClusterCAKeyFile, installspec.ClusterCACertFile, controllerKeyFile, controllerCertFile} {
		if _, err := os.Stat(filepath.Join(dir, f)); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
	if clusterca.CAKeyFile != installspec.ClusterCAKeyFile || clusterca.CACertFile != installspec.ClusterCACertFile {
		t.Error("clusterca and installspec disagree on the CA file names")
	}
	issued := func(o *Orchestrator) []SecurityEvent {
		es, err := o.SecurityLog(context.Background(), SecurityFilter{Action: securitylog.CertIssue})
		must(t, err)
		return es
	}
	es := issued(o)
	if len(es) != 1 || es[0].Detail != clusterca.RoleController {
		t.Fatalf("cert.issue events = %+v, want one for the controller", es)
	}
	if _, err := o.store.NodeCerts.Get(context.Background(), es[0].TargetID); err != nil {
		t.Errorf("no node_certs row for the serial in the log: %v", err)
	}
	ca1, _ := os.ReadFile(filepath.Join(dir, installspec.ClusterCACertFile))
	must(t, o.Close())
	o2, err := New(Config{DataDir: dir, SecretsKey: testKey, Conntrack: dir + "/nf_conntrack"},
		WithDocker(dockerfake.New()), WithVIP(vipStub{}),
		WithProxy(func(context.Context, json.RawMessage) error { return nil }))
	must(t, err)
	t.Cleanup(func() { _ = o2.Close() })
	ca2, _ := os.ReadFile(filepath.Join(dir, installspec.ClusterCACertFile))
	if string(ca1) != string(ca2) || len(issued(o2)) != 1 {
		t.Error("a second boot made a new CA or a new controller cert")
	}
}

// A worker node row gets a router entry that reaches the worker over mTLS; a
// deploy to an env pinned to that node goes through it, not to the
// controller's daemon.
func TestDeployToAWorkerGoesThroughIt(t *testing.T) {
	ctx := context.Background()
	o := newClusterPanel(t)
	daemon, _, _ := workerNode(t, o, "w1")
	must(t, o.refreshNodeClients(ctx))

	hi, err := o.docker.HostInfo(WithNode(ctx, "w1"))
	must(t, err)
	if hi.CPUs != 6 {
		t.Fatalf("host info = %+v, want the worker daemon's", hi)
	}

	org, stack, now := uuid.NewString(), uuid.NewString(), time.Now()
	must(t, o.store.Orgs.Create(ctx, store.Org{ID: org, Name: "acme", Slug: "acme", EnvColors: "{}", Settings: "{}", CreatedAt: now}))
	must(t, o.store.Stacks.Create(ctx, store.Stack{ID: stack, OrgID: org, Name: "shop", Slug: "shop", Settings: "{}", CreatedAt: now}))
	env, err := o.CreateEnv(ctx, stack, "edge", EnvSpec{NodeID: "w1", Type: environment.Static, FromKind: environment.FromBranch, FromBranch: "main"})
	must(t, err)
	if env.NodeID != "w1" {
		t.Fatalf("env on %q", env.NodeID)
	}
	tl, err := o.CreateTile(ctx, Tile{StackID: stack, EnvironmentID: env.ID, Name: "web", Kind: tile.Image, ImageRef: "nginx:1", ContainerPort: 80})
	must(t, err)
	j, err := o.Deploy(ctx, tl.ID)
	must(t, err)
	for range 500 { // the job fails on the thin daemon; it only has to get there
		if j, err = o.GetJob(ctx, j.ID); err != nil || j.State != job.Queued && j.State != job.Running {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !daemon.saw("POST /v1.45/networks/create") {
		t.Errorf("the env network was not made through the worker: %v", daemon.reqs)
	}
	if es := o.extrasOrNil("w1"); es == nil {
		t.Error("no extras client for the worker node")
	}
}

func (o *Orchestrator) extrasOrNil(id string) any {
	c, err := o.nodeExtras(id)
	if err != nil {
		return nil
	}
	return c
}

// A node that goes offline loses its entry (calls answer 503); a revoked
// worker cert is refused by the controller's dial.
func TestNodeClientsFollowTheRows(t *testing.T) {
	ctx := context.Background()
	o := newClusterPanel(t)
	_, srv1, _ := workerNode(t, o, "w1")
	must(t, o.refreshNodeClients(ctx))
	if _, err := o.docker.HostInfo(WithNode(ctx, "w1")); err != nil {
		t.Fatal(err)
	}

	// Revoked before its first handshake (a live connection stays up).
	_, _, iss2 := workerNode(t, o, "w2")
	must(t, o.store.NodeCerts.Create(ctx, store.NodeCert{Serial: iss2.Serial, NodeID: "w2", Role: iss2.Role, NotAfter: iss2.NotAfter, CreatedAt: time.Now()}))
	must(t, o.store.NodeCerts.Revoke(ctx, iss2.Serial, time.Now()))
	must(t, o.refreshNodeClients(ctx))
	if _, err := o.docker.HostInfo(WithNode(ctx, "w2")); err == nil {
		t.Error("a revoked worker cert was accepted")
	}

	// A worker that stops answering goes offline after two missed refreshes.
	srv1.Close()
	must(t, o.refreshNodeClients(ctx))
	if n, _ := o.store.Nodes.Get(ctx, "w1"); n.Status != node.StatusOnline {
		t.Fatalf("one miss made the node %s", n.Status)
	}
	must(t, o.refreshNodeClients(ctx))
	if n, _ := o.store.Nodes.Get(ctx, "w1"); n.Status != node.StatusOffline {
		t.Fatalf("status = %s, want offline", n.Status)
	}
	if _, err := o.docker.HostInfo(WithNode(ctx, "w1")); !errors.Is(err, errs.ErrBusy) {
		t.Errorf("offline node = %v, want ErrBusy", err)
	}
	if _, err := o.nodeExtras("w1"); !errors.Is(err, errs.ErrBusy) {
		t.Errorf("extras of an offline node = %v, want ErrBusy", err)
	}
}
