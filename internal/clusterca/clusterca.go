// Package clusterca is the cluster's small private CA: one self-signed CA,
// short-lived controller and worker (node) certs issued from CSRs, and the
// mTLS configs that check them. Standard library only, so the worker can
// import it. The role lives in the cert subject OU, the node id in the CN.
package clusterca

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

const (
	RoleController = "controller"
	RoleWorker     = "worker"

	CAKeyFile  = "keys/cluster-ca.key"
	CACertFile = "keys/cluster-ca.crt"
	// CAPrevCertFile is the previous root during a rollover: cert only, the
	// controller never signs with it again.
	CAPrevCertFile = "keys/cluster-ca.prev.crt"

	CALifetime         = 10 * 365 * 24 * time.Hour
	ControllerLifetime = 90 * 24 * time.Hour
	NodeLifetime       = 90 * 24 * time.Hour
	// ClockSlack widens every NotBefore and NotAfter check.
	ClockSlack = time.Hour
)

// CA is the cluster CA: its cert and signing key.
type CA struct {
	Cert *x509.Certificate
	Key  crypto.Signer
}

// Issued is one freshly signed cert. NodeID is empty for the controller.
type Issued struct {
	CertPEM  []byte
	Serial   string // hex, the node_certs key
	Issuer   string // Fingerprint of the root that signed it
	NodeID   string
	Role     string
	NotAfter time.Time
}

// Peer is who a verified cert says it is.
type Peer struct {
	Role   string
	NodeID string
	Serial string
	Issuer string // Fingerprint of the root that vouched for it
}

// Roots is the trust list: the roots a cert may chain to. It is a func so a
// listener reads the live list per handshake and a rollover applies without
// a restart. One root behaves as a lone CA always did.
type Roots func() []*x509.Certificate

// StaticRoots is a fixed trust list.
func StaticRoots(roots ...*x509.Certificate) Roots {
	return func() []*x509.Certificate { return roots }
}

// Fingerprint is the hex SHA-256 of the cert's DER: how a root is named in
// join commands, the store and node_certs.issuer.
func Fingerprint(c *x509.Certificate) string {
	h := sha256.Sum256(c.Raw)
	return hex.EncodeToString(h[:])
}

func newSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
}

// NewKey makes a P-256 key, for the CA, the controller or a node.
func NewKey() (*ecdsa.PrivateKey, error) {
	return ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
}

// NewCA makes a fresh CA valid from now (minus slack) for CALifetime.
func NewCA(now time.Time) (*CA, error) {
	key, err := NewKey()
	if err != nil {
		return nil, err
	}
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	tpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: "stackr cluster CA"},
		NotBefore:             now.Add(-ClockSlack),
		NotAfter:              now.Add(CALifetime),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, key.Public(), key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key}, nil
}

// NewCSR makes a PEM CSR for key. The CA ignores its subject.
func NewCSR(key crypto.Signer) ([]byte, error) {
	der, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{}, key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: der}), nil
}

// IssueController signs a controller cert for the CSR's public key.
func (ca *CA) IssueController(csrPEM []byte, now time.Time) (Issued, error) {
	return ca.issue(csrPEM, RoleController, "", ControllerLifetime, now)
}

// IssueNode signs a worker cert carrying nodeID for the CSR's public key.
func (ca *CA) IssueNode(csrPEM []byte, nodeID string, now time.Time) (Issued, error) {
	if nodeID == "" {
		return Issued{}, errors.New("clusterca: node id required")
	}
	return ca.issue(csrPEM, RoleWorker, nodeID, NodeLifetime, now)
}

func (ca *CA) issue(csrPEM []byte, role, nodeID string, life time.Duration, now time.Time) (Issued, error) {
	blk, _ := pem.Decode(csrPEM)
	if blk == nil || blk.Type != "CERTIFICATE REQUEST" {
		return Issued{}, errors.New("clusterca: not a PEM CSR")
	}
	csr, err := x509.ParseCertificateRequest(blk.Bytes)
	if err != nil {
		return Issued{}, fmt.Errorf("clusterca: parse CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return Issued{}, fmt.Errorf("clusterca: CSR signature: %w", err)
	}
	serial, err := newSerial()
	if err != nil {
		return Issued{}, err
	}
	cn := nodeID
	if role == RoleController {
		cn = RoleController
	}
	notAfter := now.Add(life)
	tpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: cn, OrganizationalUnit: []string{role}},
		NotBefore:    now.Add(-ClockSlack),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, ca.Cert, csr.PublicKey, ca.Key)
	if err != nil {
		return Issued{}, err
	}
	return Issued{
		CertPEM:  pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		Serial:   hex.EncodeToString(serial.Bytes()),
		Issuer:   Fingerprint(ca.Cert),
		NodeID:   nodeID,
		Role:     role,
		NotAfter: notAfter,
	}, nil
}

// Verify checks that chain[0] was signed by one of roots, that root and the
// leaf are inside their validity windows (with ClockSlack either side), the
// leaf is usable for TLS, and carries role. A non-empty nodeID must match
// too. Only the leaf is read: peers send no intermediates because there are
// none. A one-root list is the plain single-CA check.
func Verify(roots []*x509.Certificate, chain []*x509.Certificate, role, nodeID string, now time.Time) (Peer, error) {
	if len(chain) == 0 {
		return Peer{}, errors.New("clusterca: no certificate")
	}
	leaf := chain[0]
	var ca *x509.Certificate
	sigErr := errors.New("no root to check against")
	var timeErr error
	for _, r := range roots {
		if err := leaf.CheckSignatureFrom(r); err != nil {
			sigErr = err
			continue
		}
		if err := checkTime(r, now); err != nil {
			timeErr = fmt.Errorf("clusterca: CA %w", err)
			continue
		}
		ca = r
		break
	}
	if ca == nil {
		if timeErr != nil {
			return Peer{}, timeErr
		}
		return Peer{}, fmt.Errorf("clusterca: not signed by the cluster CA: %w", sigErr)
	}
	if leaf.IsCA {
		return Peer{}, errors.New("clusterca: leaf is a CA")
	}
	if err := checkTime(leaf, now); err != nil {
		return Peer{}, fmt.Errorf("clusterca: %w", err)
	}
	var client, server bool
	for _, u := range leaf.ExtKeyUsage {
		client = client || u == x509.ExtKeyUsageClientAuth
		server = server || u == x509.ExtKeyUsageServerAuth
	}
	if !client || !server {
		return Peer{}, errors.New("clusterca: cert is not for TLS")
	}
	if len(leaf.Subject.OrganizationalUnit) != 1 {
		return Peer{}, errors.New("clusterca: cert has no single role")
	}
	got := leaf.Subject.OrganizationalUnit[0]
	if got != role {
		return Peer{}, fmt.Errorf("clusterca: role is %q, want %q", got, role)
	}
	p := Peer{Role: got, Serial: hex.EncodeToString(leaf.SerialNumber.Bytes()), Issuer: Fingerprint(ca)}
	if got == RoleWorker {
		p.NodeID = leaf.Subject.CommonName
	}
	if nodeID != "" && p.NodeID != nodeID {
		return Peer{}, fmt.Errorf("clusterca: node id is %q, want %q", p.NodeID, nodeID)
	}
	return p, nil
}

func checkTime(c *x509.Certificate, now time.Time) error {
	if now.Add(ClockSlack).Before(c.NotBefore) {
		return errors.New("certificate not valid yet")
	}
	if now.Add(-ClockSlack).After(c.NotAfter) {
		return errors.New("certificate expired")
	}
	return nil
}

// NeedsRenewal is true once a third of the cert's lifetime or less is left.
func NeedsRenewal(c *x509.Certificate, now time.Time) bool {
	return c.NotAfter.Sub(now) <= c.NotAfter.Sub(c.NotBefore)/3
}

// verifier returns the tls VerifyPeerCertificate hook; roots is read per
// handshake. revoked may be nil.
func verifier(roots Roots, role, nodeID string, revoked func(serial string) bool) func([][]byte, [][]*x509.Certificate) error {
	return func(raw [][]byte, _ [][]*x509.Certificate) error {
		chain := make([]*x509.Certificate, 0, len(raw))
		for _, r := range raw {
			c, err := x509.ParseCertificate(r)
			if err != nil {
				return err
			}
			chain = append(chain, c)
		}
		p, err := Verify(roots(), chain, role, nodeID, time.Now())
		if err != nil {
			return err
		}
		if revoked != nil && revoked(p.Serial) {
			return fmt.Errorf("clusterca: cert %s is revoked", p.Serial)
		}
		return nil
	}
}

// ServerConfig is the worker's listener config: it presents own and demands
// a client cert from the trust list with role controller. The chain check is ours
// (RequireAnyClientCert) so the clock slack applies.
func ServerConfig(ca Roots, own tls.Certificate, revoked func(serial string) bool) *tls.Config {
	return &tls.Config{
		MinVersion:            tls.VersionTLS13,
		Certificates:          []tls.Certificate{own},
		ClientAuth:            tls.RequireAnyClientCert,
		VerifyPeerCertificate: verifier(ca, RoleController, "", revoked),
	}
}

// ClientConfig is the controller's dial config: it presents own and verifies
// the worker cert against the trust list. nodeID, when not empty, pins one node. The
// hostname check is off (nodes are dialled by address); identity is the
// cert's role and node id, checked by the verify hook.
func ClientConfig(ca Roots, own tls.Certificate, nodeID string, revoked func(serial string) bool) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{own},
		// Safe: VerifyPeerCertificate below does the whole check.
		InsecureSkipVerify:    true,
		VerifyPeerCertificate: verifier(ca, RoleWorker, nodeID, revoked),
	}
}

// WorkerServerConfig is the controller's listener on the mesh (the registry
// front): it presents own, read per handshake so a renewal applies, and
// demands a worker cert from the trust list.
func WorkerServerConfig(ca Roots, own func() tls.Certificate, revoked func(serial string) bool) *tls.Config {
	return &tls.Config{
		MinVersion: tls.VersionTLS13,
		GetCertificate: func(*tls.ClientHelloInfo) (*tls.Certificate, error) {
			c := own()
			return &c, nil
		},
		ClientAuth:            tls.RequireAnyClientCert,
		VerifyPeerCertificate: verifier(ca, RoleWorker, "", revoked),
	}
}

// ControllerClientConfig is a worker's dial config to the controller: it
// presents own and verifies the controller cert against the trust list.
func ControllerClientConfig(ca Roots, own tls.Certificate) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{own},
		// Safe: VerifyPeerCertificate below does the whole check.
		InsecureSkipVerify:    true,
		VerifyPeerCertificate: verifier(ca, RoleController, "", nil),
	}
}

// KeyPEM encodes a private key as PKCS#8 PEM.
func KeyPEM(key crypto.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// ParseKeyPEM is the inverse of KeyPEM.
func ParseKeyPEM(b []byte) (crypto.Signer, error) {
	blk, _ := pem.Decode(b)
	if blk == nil {
		return nil, errors.New("clusterca: not PEM")
	}
	k, err := x509.ParsePKCS8PrivateKey(blk.Bytes)
	if err != nil {
		return nil, err
	}
	s, ok := k.(crypto.Signer)
	if !ok {
		return nil, errors.New("clusterca: key cannot sign")
	}
	return s, nil
}

// ParseCertPEM reads the first certificate in b.
func ParseCertPEM(b []byte) (*x509.Certificate, error) {
	blk, _ := pem.Decode(b)
	if blk == nil || blk.Type != "CERTIFICATE" {
		return nil, errors.New("clusterca: not a PEM certificate")
	}
	return x509.ParseCertificate(blk.Bytes)
}

// SaveCA writes keys/cluster-ca.key (0600) and keys/cluster-ca.crt under dir,
// each aside first and renamed in.
func SaveCA(dir string, ca *CA) error {
	k, err := KeyPEM(ca.Key)
	if err != nil {
		return err
	}
	c := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Cert.Raw})
	if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(CAKeyFile)), 0o700); err != nil {
		return err
	}
	return writeAside(dir, []file{{CAKeyFile, k, 0o600}, {CACertFile, c, 0o644}})
}

type file struct {
	name string
	b    []byte
	mode os.FileMode
}

// writeAside writes every file as name.new, then renames each into place: no
// reader sees a half-written file. The pair is torn only between the renames;
// callers that care (the panel backup) hold a lock over that.
func writeAside(dir string, fs []file) error {
	for _, f := range fs {
		if err := os.WriteFile(filepath.Join(dir, f.name+".new"), f.b, f.mode); err != nil {
			return err
		}
	}
	for _, f := range fs {
		if err := os.Rename(filepath.Join(dir, f.name+".new"), filepath.Join(dir, f.name)); err != nil {
			return err
		}
	}
	return nil
}

// LoadCA reads what SaveCA wrote. A missing file is an os.ErrNotExist error.
func LoadCA(dir string) (*CA, error) {
	kb, err := os.ReadFile(filepath.Join(dir, CAKeyFile))
	if err != nil {
		return nil, err
	}
	cb, err := os.ReadFile(filepath.Join(dir, CACertFile))
	if err != nil {
		return nil, err
	}
	key, err := ParseKeyPEM(kb)
	if err != nil {
		return nil, err
	}
	cert, err := ParseCertPEM(cb)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key}, nil
}

// LoadPrevCA reads the previous root's cert, kept while a rollover runs. A
// missing file is an os.ErrNotExist error: there is no rollover.
func LoadPrevCA(dir string) (*x509.Certificate, error) {
	b, err := os.ReadFile(filepath.Join(dir, CAPrevCertFile))
	if err != nil {
		return nil, err
	}
	return ParseCertPEM(b)
}

// RotateCA makes next the current root and keeps the current one's cert as
// the previous root (cert only). The previous cert is written first: a crash
// between the two writes leaves the old root current and listed as previous
// too, which the caller reads as nothing rotated (service.reconcileRollover). Rolling over twice in a row loses the first previous root, so
// callers finish a rollover before starting another.
func RotateCA(dir string, next *CA) error {
	cur, err := LoadCA(dir)
	if err != nil {
		return err
	}
	b := BundlePEM([]*x509.Certificate{cur.Cert})
	if err := writeAside(dir, []file{{CAPrevCertFile, b, 0o644}}); err != nil {
		return err
	}
	return SaveCA(dir, next)
}

// DropPrevCA deletes the previous root: the rollover is over. A missing file
// is fine.
func DropPrevCA(dir string) error {
	err := os.Remove(filepath.Join(dir, CAPrevCertFile))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

// TrustRoots is the controller's trust list: the current root, then the
// previous one while a rollover runs.
func TrustRoots(dir string) ([]*x509.Certificate, error) {
	ca, err := LoadCA(dir)
	if err != nil {
		return nil, err
	}
	roots := []*x509.Certificate{ca.Cert}
	prev, err := LoadPrevCA(dir)
	switch {
	case err == nil:
		roots = append(roots, prev)
	case !errors.Is(err, os.ErrNotExist):
		return nil, err
	}
	return roots, nil
}

// BundlePEM encodes certs as one PEM bundle, in order.
func BundlePEM(certs []*x509.Certificate) []byte {
	var b []byte
	for _, c := range certs {
		b = append(b, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: c.Raw})...)
	}
	return b
}

// ParseBundlePEM reads every certificate in b; any other block is an error,
// and so is an empty bundle.
func ParseBundlePEM(b []byte) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	for {
		var blk *pem.Block
		blk, b = pem.Decode(b)
		if blk == nil {
			break
		}
		if blk.Type != "CERTIFICATE" {
			return nil, fmt.Errorf("clusterca: unexpected %q block", blk.Type)
		}
		c, err := x509.ParseCertificate(blk.Bytes)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if len(out) == 0 {
		return nil, errors.New("clusterca: no certificate")
	}
	return out, nil
}

// RootsToPEM is the wire form of a trust list: one PEM string per root.
func RootsToPEM(roots []*x509.Certificate) []string {
	out := make([]string, 0, len(roots))
	for _, r := range roots {
		out = append(out, string(BundlePEM([]*x509.Certificate{r})))
	}
	return out
}

// RootsFromPEM is the inverse of RootsToPEM. An empty list is no error: it
// means "no trust list sent".
func RootsFromPEM(pems []string) ([]*x509.Certificate, error) {
	var out []*x509.Certificate
	for _, p := range pems {
		cs, err := ParseBundlePEM([]byte(p))
		if err != nil {
			return nil, err
		}
		out = append(out, cs...)
	}
	return out, nil
}
