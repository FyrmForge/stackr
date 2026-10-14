package worker

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/clusterca"
)

// Node cert renewal: in its last third (clusterca.NeedsRenewal) the worker
// makes a new key, sends a CSR to the controller over the tunnel with its
// current cert, and swaps to the new pair. With the tunnel down it signs the
// same CSR with its current key and sends it to the panel's public URL, the
// recovery path. The key never leaves the node. The old pair stays on disk
// as node.{crt,key}.prev until the next renewal; the controller lets the old
// cert run out rather than revoking it, so a lost answer strands nobody.
const (
	// RenewTunnelPath is the controller's renewal endpoint on its mesh
	// listener (the registry front's address), worker certs only.
	RenewTunnelPath = "/stackr/v1/cert/renew"
	// RenewPanelPath is the panel's public renewal endpoint, signed like a
	// recovery call.
	RenewPanelPath = "/api/v1/nodes/renew"
	// renewEvery is how often the worker checks its cert.
	renewEvery = time.Hour

	certFile, keyFile = "node.crt", "node.key"
)

// RenewRequest is a CSR sent over the tunnel; RenewAnswer the signed cert.
type RenewRequest struct {
	CSR string `json:"csr"`
}
type RenewAnswer struct {
	Cert string `json:"cert"`
}

// ownCert is the node's current cert, swapped in place on renewal: the
// listener, the registry proxy and recovery read it per use.
type ownCert struct {
	mu sync.RWMutex
	c  tls.Certificate
}

func (o *ownCert) get() tls.Certificate {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return o.c
}

func (o *ownCert) set(c tls.Certificate) {
	o.mu.Lock()
	o.c = c
	o.mu.Unlock()
}

// ownCert is the node's current cert; before one is known, the recovery
// config's.
func (s *Server) ownCert() tls.Certificate {
	if c := s.own.get(); len(c.Certificate) > 0 {
		return c
	}
	return s.cfg.Recover.Own
}

func (s *Server) serverCert(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	c := s.own.get()
	return &c, nil
}

func (s *Server) clientCert(*tls.CertificateRequestInfo) (*tls.Certificate, error) {
	c := s.own.get()
	return &c, nil
}

// withOwn is tc presenting the node's current cert as a client.
func (s *Server) withOwn(tc *tls.Config) *tls.Config {
	tc = tc.Clone()
	tc.Certificates = nil
	tc.GetClientCertificate = s.clientCert
	return tc
}

// LoadNodeCert reads the node's pair; a pair that does not match (a crash
// between the two renames of a renewal) falls back to the .prev pair the
// renewal kept.
func LoadNodeCert(cert, key string) (tls.Certificate, error) {
	c, err := tls.LoadX509KeyPair(cert, key)
	if err == nil {
		return c, nil
	}
	if p, perr := tls.LoadX509KeyPair(cert+".prev", key+".prev"); perr == nil {
		slog.Warn("worker: node cert unreadable, using the previous pair", "err", err)
		return p, nil
	}
	return tls.Certificate{}, err
}

// renewLoop checks the cert at start and every renewEvery.
func (s *Server) renewLoop() {
	t := time.NewTicker(renewEvery)
	defer t.Stop()
	for {
		if err := s.renewOnce(s.ctx, time.Now()); err != nil {
			slog.Warn("worker: node cert renewal", "err", err)
		}
		select {
		case <-s.ctx.Done():
			return
		case <-t.C:
		case <-s.renewKick:
		}
	}
}

// applyTrust takes a spec's trust fields; a new signing root (a rollover
// started) wakes the cert check now instead of at the next hourly tick.
func (s *Server) applyTrust(roots, serials []string) error {
	before := s.signingRoot()
	if err := s.cfg.Trust.Apply(roots, serials); err != nil {
		return err
	}
	if s.signingRoot() != before {
		select {
		case s.renewKick <- struct{}{}:
		default:
		}
	}
	return nil
}

// signingRoot is the fingerprint of the first root in the trust list.
func (s *Server) signingRoot() string {
	if r := s.cfg.Trust.Roots(); len(r) > 0 {
		return clusterca.Fingerprint(r[0])
	}
	return ""
}

// renewOnce renews when the cert is due: tunnel first, then the panel.
func (s *Server) renewOnce(ctx context.Context, now time.Time) error {
	cur := s.own.get()
	if len(cur.Certificate) == 0 {
		return nil
	}
	leaf, err := x509.ParseCertificate(cur.Certificate[0])
	if err != nil || !clusterca.NeedsRenewal(leaf, now) && !s.renewNow.Swap(false) && !s.offSigningRoot(leaf, now) {
		return err
	}
	roots := s.cfg.Trust.Roots()
	old, err := clusterca.Verify(roots, []*x509.Certificate{leaf}, clusterca.RoleWorker, "", now)
	if err != nil {
		return fmt.Errorf("the node cert can no longer be renewed (%w): rejoin the node", err)
	}
	key, err := clusterca.NewKey()
	if err != nil {
		return err
	}
	csr, err := clusterca.NewCSR(key)
	if err != nil {
		return err
	}
	certPEM, err := s.renewTunnel(ctx, csr)
	via := "tunnel"
	if err != nil {
		slog.Warn("worker: cert renewal over the tunnel failed, trying the panel", "err", err)
		certPEM, err = s.renewPanel(ctx, csr)
		via = "panel"
	}
	if err != nil {
		return err
	}
	kb, err := clusterca.KeyPEM(key)
	if err != nil {
		return err
	}
	pair, err := tls.X509KeyPair(certPEM, kb)
	if err != nil {
		return fmt.Errorf("renewed cert does not match the new key: %w", err)
	}
	nl, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return err
	}
	if _, err := clusterca.Verify(roots, []*x509.Certificate{nl}, clusterca.RoleWorker, old.NodeID, now); err != nil {
		return fmt.Errorf("renewed cert refused: %w", err)
	}
	if err := s.swapCertFiles(certPEM, kb); err != nil {
		return err
	}
	s.own.set(pair)
	slog.Info("worker: node cert renewed", "via", via, "until", nl.NotAfter.UTC().Format(time.DateOnly))
	return nil
}

// offSigningRoot is true during a root rollover when the cert was issued by
// an older root than the first one in the trust list (the root the controller
// signs with now): the node then renews at once instead of waiting for its
// last third, and keeps asking hourly until it holds a cert from that root.
func (s *Server) offSigningRoot(leaf *x509.Certificate, now time.Time) bool {
	roots := s.cfg.Trust.Roots()
	if len(roots) < 2 {
		return false
	}
	p, err := clusterca.Verify(roots, []*x509.Certificate{leaf}, clusterca.RoleWorker, "", now)
	return err == nil && p.Issuer != clusterca.Fingerprint(roots[0])
}

// swapCertFiles keeps the current pair as .prev, then renames the new pair
// in, key last; LoadNodeCert covers a crash in between.
func (s *Server) swapCertFiles(certPEM, keyPEM []byte) error {
	d := s.cfg.StateDir
	for _, f := range []string{certFile, keyFile} {
		if b, err := os.ReadFile(filepath.Join(d, f)); err == nil {
			if err := os.WriteFile(filepath.Join(d, f+".prev"), b, 0o600); err != nil {
				return err
			}
		}
	}
	if err := os.WriteFile(filepath.Join(d, certFile+".new"), certPEM, 0o644); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(d, keyFile+".new"), keyPEM, 0o600); err != nil {
		return err
	}
	if err := os.Rename(filepath.Join(d, certFile+".new"), filepath.Join(d, certFile)); err != nil {
		return err
	}
	return os.Rename(filepath.Join(d, keyFile+".new"), filepath.Join(d, keyFile))
}

// controllerURL is the controller's mesh listener: the flag's registry URL,
// else the one the applied spec names.
func (s *Server) controllerURL() string {
	if s.cfg.Registry.URL != "" {
		return s.cfg.Registry.URL
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mesh.applied != nil && s.mesh.applied.Registry != nil {
		return s.mesh.applied.Registry.URL
	}
	return ""
}

func (s *Server) renewTunnel(ctx context.Context, csr []byte) ([]byte, error) {
	u := strings.TrimRight(s.controllerURL(), "/")
	if u == "" {
		return nil, errors.New("no controller address known yet")
	}
	tc := s.withOwn(s.cfg.Trust.Guard(clusterca.ControllerClientConfig(s.cfg.Trust.Roots, tls.Certificate{})))
	body, _ := json.Marshal(RenewRequest{CSR: string(csr)})
	return renewPost(ctx, &http.Client{Transport: &http.Transport{TLSClientConfig: tc}}, u+RenewTunnelPath, body)
}

func (s *Server) renewPanel(ctx context.Context, csr []byte) ([]byte, error) {
	panel := strings.TrimRight(s.panelURL(), "/")
	if panel == "" {
		return nil, errors.New("no panel URL known")
	}
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	req, err := SignRecover(s.ownCert(), RecoverPayload{Time: time.Now().Unix(), Nonce: hex.EncodeToString(nonce), Purpose: PurposeRenew, CSR: string(csr)})
	if err != nil {
		return nil, err
	}
	body, _ := json.Marshal(req)
	return renewPost(ctx, s.cfg.Recover.client(), panel+RenewPanelPath, body)
}

func renewPost(ctx context.Context, c *http.Client, url string, body []byte) ([]byte, error) {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	hr, err := http.NewRequestWithContext(cctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := c.Do(hr)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("answered %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	var a RenewAnswer
	if err := json.Unmarshal(b, &a); err != nil || a.Cert == "" {
		return nil, fmt.Errorf("renew answer: %v", err)
	}
	return []byte(a.Cert), nil
}

// Trust is the worker's live view of who the cluster trusts, kept in files
// next to the CA file from join: the trust list (every root, one PEM bundle)
// and the controller allow list (hex serials, one per line). TLS configs read
// it per handshake, so a push applies without a restart; both files are
// written temp + rename and read back at start, so a restart keeps enforcing
// them.
type Trust struct {
	rootsFile, serialsFile string
	mu                     sync.RWMutex
	roots                  []*x509.Certificate
	serials                map[string]bool // nil = no allow list
}

const (
	trustRootsFile   = "trust-roots.pem"
	trustSerialsFile = "controller-serials"
)

// NewTrust loads the trust list next to caFile: the list file when there is
// one, else the join-time CA. A missing or unreadable CA leaves the list
// empty, so nothing verifies.
func NewTrust(caFile string) *Trust {
	dir := filepath.Dir(caFile)
	t := &Trust{rootsFile: filepath.Join(dir, trustRootsFile), serialsFile: filepath.Join(dir, trustSerialsFile)}
	for _, f := range []string{t.rootsFile, caFile} {
		if b, err := os.ReadFile(f); err == nil {
			if roots, err := clusterca.ParseBundlePEM(b); err == nil && len(roots) > 0 {
				t.roots = roots
				break
			}
		}
	}
	if b, err := os.ReadFile(t.serialsFile); err == nil {
		t.serials = serialSet(strings.Fields(string(b)))
	}
	return t
}

// ResetTrust starts the trust files next to caFile over from a join answer:
// a rejoin must not keep the roots and allow list of before (NewTrust
// prefers the list file to the CA file). No roots = the CA file alone.
func ResetTrust(caFile string, roots, serials []string) error {
	dir := filepath.Dir(caFile)
	for _, f := range []string{trustRootsFile, trustSerialsFile} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return NewTrust(caFile).Apply(roots, serials)
}

// Roots is the live trust list, for clusterca's TLS configs.
func (t *Trust) Roots() []*x509.Certificate {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.roots
}

// Refuses reports a controller serial (hex) the allow list leaves out; with
// no list nothing is refused. The revoked hook of clusterca.ServerConfig.
func (t *Trust) Refuses(serial string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.serials != nil && !t.serials[normSerial(serial)]
}

// Guard adds the allow list to a client config that dials the controller
// (clusterca.ControllerClientConfig has no revoked hook).
func (t *Trust) Guard(tc *tls.Config) *tls.Config {
	inner := tc.VerifyPeerCertificate
	tc.VerifyPeerCertificate = func(raw [][]byte, chains [][]*x509.Certificate) error {
		if err := inner(raw, chains); err != nil {
			return err
		}
		c, err := x509.ParseCertificate(raw[0])
		if err != nil {
			return err
		}
		if t.Refuses(hex.EncodeToString(c.SerialNumber.Bytes())) {
			return errors.New("worker: controller cert is not on the allow list")
		}
		return nil
	}
	return tc
}

// Apply takes the trust fields of a mesh spec. No roots = keep the current
// list (a spec that omits it must not wipe trust). The allow list is taken as
// sent, except that an empty one never replaces a set one; before any is set,
// empty means none (any controller cert from a trusted root). Nothing is
// changed when a root does not parse.
func (t *Trust) Apply(rootsPEM, serials []string) error {
	var roots []*x509.Certificate
	if len(rootsPEM) > 0 {
		var err error
		if roots, err = clusterca.RootsFromPEM(rootsPEM); err != nil {
			return fmt.Errorf("trust list: %w", err)
		}
	}
	set := serialSet(serials)
	t.mu.Lock()
	defer t.mu.Unlock()
	if set == nil && t.serials != nil {
		// Once a list is set, only a non-empty one replaces it: an empty one
		// would stop a revoked controller cert from ever reaching this
		// worker. A spec with no trust fields at all (a removal push, the
		// controller's own spec) leaves trust alone.
		if len(roots) > 0 {
			return errors.New("trust: refusing an empty controller allow list once one is set")
		}
		return nil
	}
	if len(roots) > 0 {
		if err := writeAtomic(t.rootsFile, clusterca.BundlePEM(roots)); err != nil {
			return err
		}
		t.roots = roots
	}
	if set == nil {
		if err := os.Remove(t.serialsFile); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else {
		var b strings.Builder
		for s := range set {
			b.WriteString(s + "\n")
		}
		if err := writeAtomic(t.serialsFile, []byte(b.String())); err != nil {
			return err
		}
	}
	t.serials = set
	return nil
}

func serialSet(l []string) map[string]bool {
	if len(l) == 0 {
		return nil
	}
	m := make(map[string]bool, len(l))
	for _, s := range l {
		m[normSerial(s)] = true
	}
	return m
}

// normSerial is a hex serial without case or leading zeros.
func normSerial(s string) string { return strings.TrimLeft(strings.ToLower(strings.TrimSpace(s)), "0") }
