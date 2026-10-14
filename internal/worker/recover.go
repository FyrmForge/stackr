package worker

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// Recovery: a worker whose controller has gone quiet (its tunnel to the
// controller is down, or the node's own addresses changed) calls the panel's
// public URL, the way join does, reports its facts and applies the mesh spec
// it gets back. Nothing else travels this path. The request is signed with
// the node key; the controller checks the cert against the cluster CA.
const (
	// RecoverAfter is how long the controller may stay quiet: it reaches
	// every worker at least once a minute.
	RecoverAfter = 90 * time.Second
	// RecoverPath is the panel's public endpoint, under its API.
	RecoverPath = "/api/v1/nodes/recover"
)

// recoverPause is the wait after a failed call: 30 s, 2 min, then 5 min.
var recoverPause = []time.Duration{30 * time.Second, 2 * time.Minute, 5 * time.Minute}

// RecoverRequest is the body of a recovery call.
type RecoverRequest struct {
	Cert    string `json:"cert"`    // the node cert, PEM
	Payload []byte `json:"payload"` // a RecoverPayload as JSON, signed as is
	Sig     []byte `json:"sig"`     // ECDSA (ASN.1) over SHA-256 of Payload
}

// RecoverPayload is what the node signs: Time and Nonce stop a replay.
type RecoverPayload struct {
	Time  int64  `json:"time"` // unix seconds
	Nonce string `json:"nonce"`
	Facts Facts  `json:"facts"`
	// WGPublic and ExitPublic are the node's current public keys: after a
	// rotation they are how the controller learns the new ones.
	WGPublic   string `json:"wg_public,omitempty"`
	ExitPublic string `json:"exit_public,omitempty"`
	CSR        string `json:"csr,omitempty"` // a renewal (RenewPanelPath)
}

// SignRecover builds a signed request with the node's own cert and key.
func SignRecover(own tls.Certificate, p RecoverPayload) (RecoverRequest, error) {
	if len(own.Certificate) == 0 {
		return RecoverRequest{}, errors.New("worker: no node cert")
	}
	signer, ok := own.PrivateKey.(crypto.Signer)
	if !ok {
		return RecoverRequest{}, errors.New("worker: node key cannot sign")
	}
	b, err := json.Marshal(p)
	if err != nil {
		return RecoverRequest{}, err
	}
	sum := sha256.Sum256(b)
	sig, err := signer.Sign(rand.Reader, sum[:], crypto.SHA256)
	if err != nil {
		return RecoverRequest{}, err
	}
	cert := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: own.Certificate[0]})
	return RecoverRequest{Cert: string(cert), Payload: b, Sig: sig}, nil
}

// OpenRecover checks the signature against the cert the request carries and
// decodes the payload. The caller checks the cert itself (cluster CA, role,
// revocation) and Time and Nonce.
func OpenRecover(r RecoverRequest) (*x509.Certificate, RecoverPayload, error) {
	blk, _ := pem.Decode([]byte(r.Cert))
	if blk == nil {
		return nil, RecoverPayload{}, errors.New("recover: no cert")
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return nil, RecoverPayload{}, fmt.Errorf("recover: cert: %w", err)
	}
	pub, ok := c.PublicKey.(*ecdsa.PublicKey)
	sum := sha256.Sum256(r.Payload)
	if !ok || !ecdsa.VerifyASN1(pub, sum[:], r.Sig) {
		return nil, RecoverPayload{}, errors.New("recover: bad signature")
	}
	var p RecoverPayload
	if err := json.Unmarshal(r.Payload, &p); err != nil {
		return nil, RecoverPayload{}, fmt.Errorf("recover: payload: %w", err)
	}
	return c, p, nil
}

// heardController marks a call from the controller: the listener admits
// nobody else.
func (s *Server) heardController() { s.lastCtl.Store(time.Now().UnixNano()) }

// recoverLoop checks every tick whether the controller has gone quiet and,
// if so, recovers through the panel, pausing longer after each failure.
func (s *Server) recoverLoop(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	var next time.Time
	fails := 0
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-t.C:
			if now.Sub(time.Unix(0, s.lastCtl.Load())) < RecoverAfter || now.Before(next) {
				continue
			}
			if err := s.recoverOnce(s.ctx); err != nil {
				slog.Warn("worker: recover through the panel", "err", err)
				next = now.Add(recoverPause[min(fails, len(recoverPause)-1)])
				fails++
				continue
			}
			fails, next = 0, time.Time{}
		}
	}
}

// panelURL is the flag's, else the one the controller named in the last
// applied spec.
func (s *Server) panelURL() string {
	if s.cfg.Recover.Panel != "" {
		return s.cfg.Recover.Panel
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mesh.applied != nil {
		return s.mesh.applied.Panel
	}
	if sp, err := s.savedMesh(); err == nil && sp != nil {
		return sp.Panel
	}
	return ""
}

func (s *Server) recoverOnce(ctx context.Context) error {
	panel := strings.TrimRight(s.panelURL(), "/")
	if panel == "" {
		return errors.New("no panel URL known")
	}
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	wgPub, exitPub := s.publicKeys()
	req, err := SignRecover(s.ownCert(), RecoverPayload{Time: time.Now().Unix(), Nonce: hex.EncodeToString(nonce), Facts: s.Facts(ctx),
		WGPublic: wgPub, ExitPublic: exitPub})
	if err != nil {
		return err
	}
	b, _ := json.Marshal(req)
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	hr, err := http.NewRequestWithContext(cctx, http.MethodPost, panel+RecoverPath, bytes.NewReader(b))
	if err != nil {
		return err
	}
	hr.Header.Set("Content-Type", "application/json")
	resp, err := s.cfg.Recover.client().Do(hr)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxBody))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("panel answered %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var spec MeshSpec
	if err := json.Unmarshal(body, &spec); err != nil {
		return fmt.Errorf("mesh spec: %w", err)
	}
	if err := s.ApplyMesh(ctx, spec); err != nil {
		return err
	}
	slog.Info("worker: recovered the mesh through the panel")
	return nil
}

// RecoverConfig is the recovery path: the node's own cert and key, and the
// panel URL when the flag names one (else the applied spec's).
type RecoverConfig struct {
	Own    tls.Certificate
	Panel  string
	Client *http.Client // nil = a plain client (public CAs)
}

func (c RecoverConfig) client() *http.Client {
	if c.Client != nil {
		return c.Client
	}
	return http.DefaultClient
}
