// Package worker is the node program behind `stackrd worker`: one mTLS
// listener that reverse-proxies the Docker API unchanged to the local socket
// and serves the extras API v1 under /v1/ (facts, iptables, wg, saved state,
// replay, Caddy admin pass-through). Calls are only ever added to v1.
// "Worker" here is the node role, not the flow/jobs runners.
package worker

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// APIVersion is sent in VersionHeader on every response.
const (
	APIVersion    = "1"
	VersionHeader = "X-Stackr-Worker-Api"
)

// Runner runs one command, feeding stdin (may be nil), returning combined output.
type Runner interface {
	Run(ctx context.Context, stdin []byte, name string, args ...string) ([]byte, error)
}

// WGApplier applies a wg config (wgapply owns the format; it is opaque here).
type WGApplier interface {
	Apply(ctx context.Context, config []byte) error
}

type Config struct {
	Version    string      // stackr build, reported in facts
	TLS        *tls.Config // built by the caller; client certs required
	Addr       string      // listen address
	DockerSock string      // local Docker unix socket
	CaddySock  string      // Caddy admin unix socket ("" = pass-through off)
	StateDir   string
	Runner     Runner    // iptables-restore, iptables, ip
	WG         WGApplier // nil = /v1/wg answers 501
	Mesh       MeshConfig
	Registry   RegistryConfig // the controller's registry on 127.0.0.1:5000; URL "" = off
	AliveAddr  string         // public liveness listener (AlivePort); "" = off
	Recover    RecoverConfig  // recovery through the panel; no cert = off
	// Trust is the live trust list and controller allow list; nil = loaded
	// from cluster-ca.crt in StateDir.
	Trust *Trust
	// RenewCertNow renews the node cert once at start even when not due.
	RenewCertNow bool
	Conntrack    string // conntrack table for /v1/traffic ("" = /proc/net/nf_conntrack)
	// DockerUserWait bounds how long replay waits for the DOCKER-USER chain
	// (0 = 2m); DockerUserPoll is the gap between checks (0 = 1s).
	DockerUserWait time.Duration
	DockerUserPoll time.Duration
}

type Server struct {
	cfg    Config
	mu     sync.Mutex // serialises applies and replay
	ctx    context.Context
	cancel context.CancelFunc
	mesh   meshState
	// lastCtl is when the controller last called (unix nanos).
	lastCtl atomic.Int64
	own     ownCert // the node cert, swapped on renewal
	// renewNow makes the next cert check renew even when not due, once.
	renewNow atomic.Bool
	// renewKick wakes the cert check early: a new signing root arrived.
	renewKick chan struct{}
	// regErr is why the registry proxy does not hold its port, "" = it does.
	regErr atomic.Value
}

func New(cfg Config) *Server {
	if cfg.DockerUserWait == 0 {
		cfg.DockerUserWait = 2 * time.Minute
	}
	if cfg.DockerUserPoll == 0 {
		cfg.DockerUserPoll = time.Second
	}
	if cfg.Trust == nil {
		cfg.Trust = NewTrust(filepath.Join(cfg.StateDir, "cluster-ca.crt"))
	}
	s := &Server{cfg: cfg, renewKick: make(chan struct{}, 1)}
	if cfg.TLS != nil && len(cfg.TLS.Certificates) > 0 {
		s.own.set(cfg.TLS.Certificates[0])
	} else {
		s.own.set(cfg.Recover.Own)
	}
	s.renewNow.Store(cfg.RenewCertNow)
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.heardController() // a fresh start gives the controller its full window
	return s
}

func unixProxy(sock string) *httputil.ReverseProxy {
	return &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			r.Out.URL.Scheme = "http"
			r.Out.URL.Host = "local"
			r.Out.Host = "local"
		},
		// Streams (logs, events, attach) must not buffer.
		FlushInterval: -1,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			},
		},
	}
}

// Handler is the whole surface; anything outside /v1/ is the Docker API.
// Docker's own version prefix is /v1.NN/, which never matches "/v1/".
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/facts", s.facts)
	mux.HandleFunc("POST /v1/iptables", s.iptables)
	mux.HandleFunc("POST /v1/allow-tcp", s.allowTCP)
	mux.HandleFunc("POST /v1/wg", s.wg)
	mux.HandleFunc("POST /v1/mesh", s.postMesh)
	mux.HandleFunc("GET /v1/mesh", s.getMesh)
	mux.HandleFunc("POST "+RotatePath, s.postRotate)
	mux.HandleFunc("PUT /v1/state", s.putState)
	mux.HandleFunc("GET /v1/state", s.getState)
	mux.HandleFunc("POST /v1/replay", s.replay)
	mux.HandleFunc("POST /v1/probe", s.probe)
	mux.HandleFunc("GET /v1/traffic", s.traffic)
	if s.cfg.CaddySock != "" {
		mux.Handle("/v1/caddy/", http.StripPrefix("/v1/caddy", unixProxy(s.cfg.CaddySock)))
	}
	mux.Handle("/", unixProxy(s.cfg.DockerSock))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set(VersionHeader, APIVersion)
		s.heardController()
		mux.ServeHTTP(w, r)
	})
}

// Run serves until ctx ends. HTTP/1.1 only: hijacked and upgraded streams
// (attach, exec) do not exist over h2.
func Run(ctx context.Context, cfg Config) error {
	// A swap the helper never finished (Docker or the host died while it
	// waited) is put back here, then the service restarts onto the old binary.
	if ok, err := RestoreStaleSwap(SwapBinDir, time.Now()); err != nil {
		slog.Error("worker: stale swap not restored", "err", err)
	} else if ok {
		slog.Warn("worker: an unconfirmed upgrade was left pending; the old binaries are back, restarting")
		return errors.New("rolled back an unconfirmed upgrade")
	}
	// Still pending: if the helper dies before its deadline nobody else puts
	// the old binaries back, so this process does it then and exits for the
	// service manager to start the old one.
	go WatchSwap(ctx, SwapBinDir, func() {
		slog.Warn("worker: the upgrade was not confirmed by its deadline; the old binaries are back, restarting")
		os.Exit(1)
	})
	if cfg.TLS == nil {
		return errors.New("worker: tls config required")
	}
	s := New(cfg)
	defer s.Stop()
	tc := cfg.TLS.Clone()
	tc.NextProtos = []string{"http/1.1"}
	if len(s.own.get().Certificate) > 0 {
		tc.Certificates, tc.GetCertificate = nil, s.serverCert
		go s.renewLoop()
	}
	raw, err := (&net.ListenConfig{Control: freebind}).Listen(ctx, "tcp", cfg.Addr)
	if err != nil {
		return err
	}
	ln := tls.NewListener(raw, tc)
	enableAcct()
	go s.reassertLoop()
	if len(cfg.Recover.Own.Certificate) > 0 {
		go s.recoverLoop(15 * time.Second)
	}
	if cfg.Registry.URL != "" {
		if cfg.Registry.TLS != nil {
			cfg.Registry.TLS = s.withOwn(cfg.Registry.TLS)
		}
		go s.runRegistry(ctx, cfg.Registry)
	}
	if cfg.AliveAddr != "" {
		go func() {
			if err := serveAlive(ctx, cfg.AliveAddr); err != nil {
				slog.Error("worker: liveness listener", "err", err)
			}
		}()
	}
	// A restart restores the saved state (wg, routes, chains, exits) on its own.
	go func() {
		if err := s.replayState(ctx); err != nil {
			slog.Error("worker: replay at start", "err", err)
		}
	}()
	return serve(ctx, ln, s.Handler())
}

func serve(ctx context.Context, ln net.Listener, h http.Handler) error {
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 30 * time.Second}
	done := make(chan struct{})
	go func() {
		<-ctx.Done()
		sc, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(sc)
		close(done)
	}()
	err := srv.Serve(ln)
	if errors.Is(err, http.ErrServerClosed) {
		<-done
		return nil
	}
	return err
}

// The worker swap's state file lives next to the binaries it swaps. The
// helper writes "pending <deadline unix>" before it replaces them, the
// controller's explicit commit rewrites it to "committed", and the helper
// removes it after a rollback. SIGTERM, a stop or the deadline never commit.
const (
	SwapBinDir   = "/usr/local/bin"
	swapState    = ".stackr-swap"
	swapSlack    = 30 * time.Second
	swapPendingK = "pending "
	swapDoneK    = "committed"
)

// SwapBinaries are what a worker upgrade replaces together.
// ponytail: the list is fixed; a third binary in the image is one more entry.
var SwapBinaries = []string{"stackrd", "stackr-install"}

// SwapPending records that a swap is waiting for its commit until deadline.
func SwapPending(bin string, deadline time.Time) error {
	return os.WriteFile(filepath.Join(bin, swapState), []byte(swapPendingK+strconv.FormatInt(deadline.Unix(), 10)), 0o644)
}

// SwapCommit marks the pending swap as confirmed. It fails when none is pending.
func SwapCommit(bin string) error {
	if _, ok := swapDeadline(bin); !ok {
		return errors.New("no swap is waiting for a commit")
	}
	return os.WriteFile(filepath.Join(bin, swapState), []byte(swapDoneK), 0o644)
}

// SwapCommitted reports whether the swap was committed.
func SwapCommitted(bin string) bool {
	b, err := os.ReadFile(filepath.Join(bin, swapState))
	return err == nil && string(b) == swapDoneK
}

// SwapClear removes the state after a rollback.
func SwapClear(bin string) { _ = os.Remove(filepath.Join(bin, swapState)) }

func swapDeadline(bin string) (time.Time, bool) {
	b, err := os.ReadFile(filepath.Join(bin, swapState))
	if err != nil || !strings.HasPrefix(string(b), swapPendingK) {
		return time.Time{}, false
	}
	sec, err := strconv.ParseInt(strings.TrimPrefix(string(b), swapPendingK), 10, 64)
	return time.Unix(sec, 0), err == nil
}

// WatchSwap waits out a pending swap's deadline and, if it is still not
// committed then, restores the old binaries and calls restored.
func WatchSwap(ctx context.Context, bin string, restored func()) {
	dl, ok := swapDeadline(bin)
	if !ok {
		return
	}
	t := time.NewTimer(time.Until(dl.Add(swapSlack)) + time.Second)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return
	case <-t.C:
	}
	if ok, err := RestoreStaleSwap(bin, time.Now()); err != nil {
		slog.Error("worker: stale swap not restored", "err", err)
	} else if ok {
		restored()
	}
}

// RestoreStaleSwap puts the .prev binaries back when a swap is still pending
// well past its deadline: the helper is gone and nobody committed.
func RestoreStaleSwap(bin string, now time.Time) (bool, error) {
	dl, ok := swapDeadline(bin)
	if !ok || now.Before(dl.Add(swapSlack)) {
		return false, nil
	}
	var errs []error
	for _, n := range SwapBinaries {
		if err := os.Rename(filepath.Join(bin, n+".prev"), filepath.Join(bin, n)); err != nil && !os.IsNotExist(err) {
			errs = append(errs, err)
		}
	}
	if err := errors.Join(errs...); err != nil {
		return false, err
	}
	SwapClear(bin)
	return true, nil
}
