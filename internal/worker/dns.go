package worker

import (
	"context"
	"crypto/tls"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/libdns/libdns"

	"github.com/FyrmForge/stackr/internal/clusterca"
)

// ACME DNS-01 goes through the controller, the only holder of DNS provider
// keys. A proxy's `dns.providers.stackr` module posts a DNSRequest to
// DNSSocket in its admin dir. On the controller stackrd answers there; on an
// ingress worker `stackrd worker` passes the call on to the controller's
// mesh listener, same paths, under the node cert.
const (
	DNSSocket     = "dns.sock"
	DNSAppendPath = "/stackr/v1/dns/append"
	DNSDeletePath = "/stackr/v1/dns/delete"
)

// DNSRequest is one append or delete: records relative to zone, libdns
// style. The answer is a DNSRequest with the records the provider touched.
type DNSRequest struct {
	Zone    string      `json:"zone"`
	Records []libdns.RR `json:"records"`
}

// serveDNS passes the proxy's DNS calls on to the controller until ctx ends.
func (s *Server) serveDNS(ctx context.Context, sock string) error {
	if err := os.MkdirAll(filepath.Dir(sock), 0o755); err != nil {
		return err
	}
	_ = os.Remove(sock) // a stale socket from the last run
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return err
	}
	return serve(ctx, ln, s.dnsForward())
}

// dnsForward sends the two DNS paths to the controller over mTLS and
// refuses everything else.
func (s *Server) dnsForward() http.Handler {
	tc := s.withOwn(s.cfg.Trust.Guard(clusterca.ControllerClientConfig(s.cfg.Trust.Roots, tls.Certificate{})))
	rp := &httputil.ReverseProxy{
		Rewrite: func(r *httputil.ProxyRequest) {
			u, _ := url.Parse(strings.TrimRight(s.controllerURL(), "/"))
			r.SetURL(u)
		},
		Transport: &http.Transport{TLSClientConfig: tc},
		ErrorHandler: func(w http.ResponseWriter, _ *http.Request, err error) {
			slog.Warn("worker: DNS call to the controller", "err", err)
			http.Error(w, "the controller did not answer: "+err.Error(), http.StatusBadGateway)
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != DNSAppendPath && r.URL.Path != DNSDeletePath {
			http.NotFound(w, r)
			return
		}
		if s.controllerURL() == "" {
			http.Error(w, "no controller address known yet", http.StatusServiceUnavailable)
			return
		}
		rp.ServeHTTP(w, r)
	})
}

// startDNS runs serveDNS next to the Caddy admin socket, when there is one.
func (s *Server) startDNS(ctx context.Context) {
	if s.cfg.CaddySock == "" {
		return
	}
	sock := filepath.Join(filepath.Dir(s.cfg.CaddySock), DNSSocket)
	go func() {
		if err := s.serveDNS(ctx, sock); err != nil && !errors.Is(err, context.Canceled) {
			slog.Error("worker: DNS socket", "sock", sock, "err", err)
		}
	}()
}
