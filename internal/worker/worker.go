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
}

func New(cfg Config) *Server {
	if cfg.DockerUserWait == 0 {
		cfg.DockerUserWait = 2 * time.Minute
	}
	if cfg.DockerUserPoll == 0 {
		cfg.DockerUserPoll = time.Second
	}
	s := &Server{cfg: cfg}
	if cfg.TLS != nil && len(cfg.TLS.Certificates) > 0 {
		s.own.set(cfg.TLS.Certificates[0])
	} else {
		s.own.set(cfg.Recover.Own)
	}
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
	mux.HandleFunc("PUT /v1/state", s.putState)
	mux.HandleFunc("GET /v1/state", s.getState)
	mux.HandleFunc("POST /v1/replay", s.replay)
	mux.HandleFunc("POST /v1/probe", s.probe)
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
	go s.reassertLoop()
	if len(cfg.Recover.Own.Certificate) > 0 {
		go s.recoverLoop(15 * time.Second)
	}
	if cfg.Registry.URL != "" {
		if cfg.Registry.TLS != nil {
			cfg.Registry.TLS = s.withOwn(cfg.Registry.TLS)
		}
		go func() {
			if err := serveRegistry(ctx, cfg.Registry); err != nil {
				slog.Error("worker: registry proxy", "err", err)
			}
		}()
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
