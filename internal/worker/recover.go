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

	"github.com/FyrmForge/stackr/internal/clusterca"
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

// GiveUpAfter is how long the panel may keep refusing a worker (403: the
// cluster no longer knows it) before the worker stops asking.
const GiveUpAfter = 24 * time.Hour

// errPanelRefused is the panel's 403: this node was removed, or its cert is
// no longer the cluster's. Matched by status, never by the body's words.
var errPanelRefused = errors.New("panel refused this node")

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
	// Purpose says which call this signature is for, so a captured renewal
	// cannot be replayed as a recovery or the other way round.
	Purpose string `json:"purpose"`
}

// The purposes a signed request can carry.
const (
	PurposeRecover = "recover"
	PurposeRenew   = "renew"
)

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

// RecoverAnswer is the panel's answer to a recovery call: the node's mesh
// spec, signed by the controller's cert for that one request. Spec is signed
// as sent (JSON of a MeshSpec).
type RecoverAnswer struct {
	Spec []byte `json:"spec"`
	Cert string `json:"cert"` // the controller cert, PEM
	Sig  []byte `json:"sig"`  // ECDSA (ASN.1) over answerDigest(request nonce, Spec)
	// PrevCert and PrevSig are the same signature by the controller's
	// previous cert, kept until it is revoked: a worker offline through one
	// renewal still holds only that serial on its allow list.
	PrevCert string `json:"prev_cert,omitempty"`
	PrevSig  []byte `json:"prev_sig,omitempty"`
	// Cross are the new root cross-signed by the old one (PEM), present only
	// while a rollover is open: a worker that trusts the old root alone can
	// then verify a controller cert from the new one.
	Cross []string `json:"cross,omitempty"`
}

// answerDigest binds the spec to the request nonce, so an answer taken off
// the wire cannot be played back to another request.
func answerDigest(nonce string, spec []byte) [32]byte {
	return sha256.Sum256(append([]byte(nonce+"\n"), spec...))
}

// signDigest signs sum with ctl and returns its cert as PEM.
func signDigest(ctl tls.Certificate, sum [32]byte) (string, []byte, error) {
	signer, ok := ctl.PrivateKey.(crypto.Signer)
	if !ok || len(ctl.Certificate) == 0 {
		return "", nil, errors.New("worker: no controller key to sign with")
	}
	sig, err := signer.Sign(rand.Reader, sum[:], crypto.SHA256)
	if err != nil {
		return "", nil, err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ctl.Certificate[0]})), sig, nil
}

// SignAnswer signs spec for the request that carried nonce with the
// controller's cert and key.
func SignAnswer(ctl tls.Certificate, nonce string, spec MeshSpec) (RecoverAnswer, error) {
	b, err := json.Marshal(spec)
	if err != nil {
		return RecoverAnswer{}, err
	}
	c, sig, err := signDigest(ctl, answerDigest(nonce, b))
	if err != nil {
		return RecoverAnswer{}, err
	}
	return RecoverAnswer{Spec: b, Cert: c, Sig: sig}, nil
}

// CoSign adds the previous controller cert's signature over the same spec.
func (a *RecoverAnswer) CoSign(prev tls.Certificate, nonce string) error {
	c, sig, err := signDigest(prev, answerDigest(nonce, a.Spec))
	if err != nil {
		return err
	}
	a.PrevCert, a.PrevSig = c, sig
	return nil
}

// Open checks the answer against what the worker trusts now: a signer whose
// controller cert chains to one of roots, whose serial refuses (the allow
// list; nil = none) does not refuse, and whose signature covers this
// request's nonce and the spec. Either signer will do, the current cert or
// the previous one, so a worker offline through one renewal gets back in; a
// revoked cert is on no list, and revoking drops the previous key, so a
// worker that took the revoke cannot be answered by the revoked key. Offline
// through two renewals, or a revoke, it needs a rejoin.
// ponytail: a worker that missed the revoke itself still lists the revoked
// serial; only break glass (a rejoin for all) closes that.
func (a RecoverAnswer) Open(roots []*x509.Certificate, refuses func(serial string) bool, nonce string, now time.Time) (MeshSpec, error) {
	// A cross cert counts as a root for this check only if a root the worker
	// trusts signed it; it is never stored.
	vouched := append([]*x509.Certificate(nil), roots...)
	for _, p := range a.Cross {
		cs, err := clusterca.ParseBundlePEM([]byte(p))
		if err != nil {
			continue
		}
		for _, x := range cs {
			for _, r := range roots {
				if x.CheckSignatureFrom(r) == nil && now.Add(clusterca.ClockSlack).After(x.NotBefore) && now.Add(-clusterca.ClockSlack).Before(x.NotAfter) {
					vouched = append(vouched, x)
					break
				}
			}
		}
	}
	sum := answerDigest(nonce, a.Spec)
	err := checkSigner(vouched, refuses, a.Cert, a.Sig, sum, now)
	if err != nil && a.PrevCert != "" {
		if checkSigner(vouched, refuses, a.PrevCert, a.PrevSig, sum, now) == nil {
			err = nil
		}
	}
	if err != nil {
		return MeshSpec{}, err
	}
	var spec MeshSpec
	if err := json.Unmarshal(a.Spec, &spec); err != nil {
		return MeshSpec{}, fmt.Errorf("mesh spec: %w", err)
	}
	return spec, nil
}

// checkSigner is one signer of an answer: a controller cert of vouched, on
// the allow list, and a good signature over sum.
func checkSigner(vouched []*x509.Certificate, refuses func(string) bool, certPEM string, sig []byte, sum [32]byte, now time.Time) error {
	blk, _ := pem.Decode([]byte(certPEM))
	if blk == nil {
		return errors.New("no controller cert")
	}
	c, err := x509.ParseCertificate(blk.Bytes)
	if err != nil {
		return fmt.Errorf("controller cert: %w", err)
	}
	if _, err := clusterca.Verify(vouched, []*x509.Certificate{c}, clusterca.RoleController, "", now); err != nil {
		return fmt.Errorf("controller cert: %w", err)
	}
	if refuses != nil && refuses(hex.EncodeToString(c.SerialNumber.Bytes())) {
		return errors.New("controller cert: not on the allow list")
	}
	pub, ok := c.PublicKey.(*ecdsa.PublicKey)
	if !ok || !ecdsa.VerifyASN1(pub, sum[:], sig) {
		return errors.New("bad signature")
	}
	return nil
}

// heardController marks a call from the controller: the listener admits
// nobody else.
func (s *Server) heardController() { s.lastCtl.Store(time.Now().UnixNano()) }

// recoverLoop checks every tick whether the controller has gone quiet and,
// if so, recovers through the panel, pausing longer after each failure.
func (s *Server) recoverLoop(every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	var st recoverState
	for {
		select {
		case <-s.ctx.Done():
			return
		case now := <-t.C:
			if s.recoverStep(now, &st) {
				return
			}
		}
	}
}

// recoverState is what the loop remembers between ticks.
type recoverState struct {
	next         time.Time // no call before this
	fails        int
	refusedSince time.Time // the first refusal of this run of them; zero = none
}

// recoverStep is one tick of the loop. It returns true when the worker should
// stop asking: the panel has refused it for GiveUpAfter. A refusal is a 403;
// any other failure (a 500, no route) is the panel's or the network's trouble
// and does not count. A success starts the count over.
func (s *Server) recoverStep(now time.Time, st *recoverState) (stop bool) {
	if now.Sub(time.Unix(0, s.lastCtl.Load())) < RecoverAfter || now.Before(st.next) {
		return false
	}
	err := s.recoverOnce(s.ctx)
	if err == nil {
		*st = recoverState{}
		return false
	}
	if errors.Is(err, errPanelRefused) {
		if st.refusedSince.IsZero() {
			st.refusedSince = now
		}
		if now.Sub(st.refusedSince) >= GiveUpAfter {
			slog.Warn("worker: removed from cluster, run stackr-install uninstall", "refused_since", st.refusedSince)
			return true
		}
	}
	slog.Warn("worker: recover through the panel", "err", err)
	st.next = now.Add(recoverPause[min(st.fails, len(recoverPause)-1)])
	st.fails++
	return false
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
	p := RecoverPayload{Time: time.Now().Unix(), Nonce: hex.EncodeToString(nonce), Purpose: PurposeRecover, Facts: s.Facts(ctx),
		WGPublic: wgPub, ExitPublic: exitPub}
	req, err := SignRecover(s.ownCert(), p)
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
	if resp.StatusCode == http.StatusForbidden {
		return fmt.Errorf("%w: %s", errPanelRefused, strings.TrimSpace(string(body)))
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("panel answered %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	// The public URL proves nothing about the cluster (STACKR_TLS=off is plain
	// HTTP): the answer is applied only when a controller cert of the roots
	// this worker already trusts, on its allow list, signed it for this very
	// request.
	var ans RecoverAnswer
	if err := json.Unmarshal(body, &ans); err != nil {
		return fmt.Errorf("recover answer: %w", err)
	}
	spec, err := ans.Open(s.cfg.Trust.Roots(), s.cfg.Trust.Refuses, p.Nonce, time.Now())
	if err != nil {
		return fmt.Errorf("recover answer: %w", err)
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
