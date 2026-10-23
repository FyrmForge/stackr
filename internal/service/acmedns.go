package service

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/libdns/libdns"

	"github.com/FyrmForge/stackr/internal/clusterca"
	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/node"
	"github.com/FyrmForge/stackr/internal/worker"
)

// acmedns.go is ACME DNS-01 through stackrd: every proxy's
// `dns.providers.stackr` module posts its challenge records here, over the
// DNS socket in the proxy admin dir on the controller, or through
// `stackrd worker` and the mesh listener from an ingress worker. The
// provider key never leaves this process.

// ACMEChallenge appends (del false) or deletes the ACME challenge records
// rs in zone through the connector that manages zone, under the zone's lock.
func (o *Orchestrator) ACMEChallenge(ctx context.Context, del bool, zone string, rs []libdns.RR) ([]libdns.RR, error) {
	row, err := o.dns.Challenge(ctx, zone, rs)
	if err != nil {
		return nil, err
	}
	_, raw, err := o.dnsAccount(ctx, row.OrgID, row.ConnectorID)
	if err != nil {
		return nil, err
	}
	p := o.dns.Locked(raw)
	in := make([]libdns.Record, len(rs))
	for i, r := range rs {
		in[i] = r
	}
	call := p.Append
	if del {
		call = p.Delete
	}
	got, err := call(ctx, row.Name, in)
	if err != nil {
		return nil, providerError{err}
	}
	out := make([]libdns.RR, len(got))
	for i, r := range got {
		out[i] = r.RR()
	}
	return out, nil
}

// providerError is the provider's own failure on a challenge call: a 502
// to the proxy, which retries, never the 4xx of a refused call.
type providerError struct{ err error }

func (e providerError) Error() string { return e.err.Error() }
func (e providerError) Unwrap() error { return e.err }

// managedDomains is every managed domain's name, for the proxy config.
func (o *Orchestrator) managedDomains(ctx context.Context) []string {
	names, err := o.dns.Names(ctx)
	if err != nil {
		slog.Warn("proxy: managed domains not read", "err", err)
	}
	return names
}

// dnsHandler serves worker.DNSAppendPath and worker.DNSDeletePath.
func (o *Orchestrator) dnsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || r.URL.Path != worker.DNSAppendPath && r.URL.Path != worker.DNSDeletePath {
		http.NotFound(w, r)
		return
	}
	var in worker.DNSRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&in); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	out, err := o.ACMEChallenge(r.Context(), r.URL.Path == worker.DNSDeletePath, in.Zone, in.Records)
	if err != nil {
		var pe providerError
		_, invalid := errs.IsInvalid(err)
		code := http.StatusBadGateway // the provider's own failure, whatever the kind made of it
		switch {
		case errors.As(err, &pe):
		case invalid:
			code = http.StatusBadRequest
		case errors.Is(err, errs.ErrRefused) || errors.Is(err, errs.ErrNotFound):
			code = http.StatusForbidden
		}
		slog.Warn("acme dns: refused", "zone", in.Zone, "err", err)
		http.Error(w, err.Error(), code)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(worker.DNSRequest{Zone: in.Zone, Records: out})
}

// meshDNS is the DNS paths on the mesh listener: an ingress worker's proxy
// through `stackrd worker`, worker cert required.
func (o *Orchestrator) meshDNS(w http.ResponseWriter, r *http.Request) {
	if r.TLS == nil || len(r.TLS.PeerCertificates) == 0 {
		http.Error(w, "worker cert required", http.StatusForbidden)
		return
	}
	peer, err := clusterca.Verify(o.cluster.roots(), r.TLS.PeerCertificates, clusterca.RoleWorker, "", time.Now())
	if err != nil {
		http.Error(w, "worker cert required", http.StatusForbidden)
		return
	}
	// Only a node that runs a proxy asks for certificates: any other
	// worker's cert must not be able to write records in a managed domain.
	n, err := o.store.Nodes.Get(r.Context(), peer.NodeID)
	if err != nil || n.Status == node.StatusPending || !slices.Contains(node.Tags(n), node.TagIngress) {
		http.Error(w, "only an ingress node may answer a DNS challenge", http.StatusForbidden)
		return
	}
	o.dnsHandler(w, r)
}

// dnsSocket is the controller's DNS socket on the host, next to Caddy's
// admin socket; "" when the admin API is not a unix socket (dev).
func (o *Orchestrator) dnsSocket() string {
	admin, ok := strings.CutPrefix(o.cfg.ProxyAdmin, "unix://")
	if !ok {
		return ""
	}
	return filepath.Join(filepath.Dir(admin), worker.DNSSocket)
}

// startDNSSocket serves the DNS socket to the controller's proxy until Close.
func (o *Orchestrator) startDNSSocket() {
	sock := o.dnsSocket()
	if sock == "" {
		return
	}
	_ = os.Remove(sock) // a stale socket from the last run
	ln, err := net.Listen("unix", sock)
	if err != nil {
		slog.Error("acme dns: socket", "sock", sock, "err", err)
		return
	}
	// A self-upgrade runs the new panel next to the old one: the old one's
	// exit must not unlink the socket the new one now serves.
	// ponytail: a new panel that fails leaves the old one without its socket
	// until it restarts.
	ln.(*net.UnixListener).SetUnlinkOnClose(false)
	srv := &http.Server{Handler: http.HandlerFunc(o.dnsHandler), ReadHeaderTimeout: 10 * time.Second}
	go func() { <-o.stopNodes; _ = srv.Close() }()
	go func() {
		if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
			slog.Error("acme dns: socket", "sock", sock, "err", err)
		}
	}()
}
