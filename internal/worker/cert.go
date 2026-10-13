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
		}
	}
}

// renewOnce renews when the cert is due: tunnel first, then the panel.
func (s *Server) renewOnce(ctx context.Context, now time.Time) error {
	cur := s.own.get()
	if len(cur.Certificate) == 0 {
		return nil
	}
	leaf, err := x509.ParseCertificate(cur.Certificate[0])
	if err != nil || !clusterca.NeedsRenewal(leaf, now) && !s.renewNow.Swap(false) {
		return err
	}
	ca, err := s.clusterCA()
	if err != nil {
		return err
	}
	old, err := clusterca.Verify([]*x509.Certificate{ca}, []*x509.Certificate{leaf}, clusterca.RoleWorker, "", now)
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
	if _, err := clusterca.Verify([]*x509.Certificate{ca}, []*x509.Certificate{nl}, clusterca.RoleWorker, old.NodeID, now); err != nil {
		return fmt.Errorf("renewed cert refused: %w", err)
	}
	if err := s.swapCertFiles(certPEM, kb); err != nil {
		return err
	}
	s.own.set(pair)
	slog.Info("worker: node cert renewed", "via", via, "until", nl.NotAfter.UTC().Format(time.DateOnly))
	return nil
}

func (s *Server) clusterCA() (*x509.Certificate, error) {
	b, err := os.ReadFile(filepath.Join(s.cfg.StateDir, "cluster-ca.crt"))
	if err != nil {
		return nil, err
	}
	return clusterca.ParseCertPEM(b)
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
	ca, err := s.clusterCA()
	if err != nil {
		return nil, err
	}
	tc := s.withOwn(clusterca.ControllerClientConfig(clusterca.StaticRoots(ca), tls.Certificate{}))
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
	req, err := SignRecover(s.ownCert(), RecoverPayload{Time: time.Now().Unix(), Nonce: hex.EncodeToString(nonce), CSR: string(csr)})
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
	defer resp.Body.Close()
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
