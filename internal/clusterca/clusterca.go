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
	NodeID   string
	Role     string
	NotAfter time.Time
}

// Peer is who a verified cert says it is.
type Peer struct {
	Role   string
	NodeID string
	Serial string
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
		NodeID:   nodeID,
		Role:     role,
		NotAfter: notAfter,
	}, nil
}

// Verify checks that chain[0] was signed by caCert, is inside its validity
// window (with ClockSlack either side), is a leaf usable for TLS, and carries
// role. A non-empty nodeID must match too. Only the leaf is read: peers send
// no intermediates because there are none.
func Verify(caCert *x509.Certificate, chain []*x509.Certificate, role, nodeID string, now time.Time) (Peer, error) {
	if len(chain) == 0 {
		return Peer{}, errors.New("clusterca: no certificate")
	}
	leaf := chain[0]
	if err := leaf.CheckSignatureFrom(caCert); err != nil {
		return Peer{}, fmt.Errorf("clusterca: not signed by the cluster CA: %w", err)
	}
	if leaf.IsCA {
		return Peer{}, errors.New("clusterca: leaf is a CA")
	}
	if err := checkTime(caCert, now); err != nil {
		return Peer{}, fmt.Errorf("clusterca: CA %w", err)
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
	p := Peer{Role: got, Serial: hex.EncodeToString(leaf.SerialNumber.Bytes())}
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

// verifier returns the tls VerifyPeerCertificate hook. revoked may be nil.
func verifier(ca *x509.Certificate, role, nodeID string, revoked func(serial string) bool) func([][]byte, [][]*x509.Certificate) error {
	return func(raw [][]byte, _ [][]*x509.Certificate) error {
		chain := make([]*x509.Certificate, 0, len(raw))
		for _, r := range raw {
			c, err := x509.ParseCertificate(r)
			if err != nil {
				return err
			}
			chain = append(chain, c)
		}
		p, err := Verify(ca, chain, role, nodeID, time.Now())
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
// a client cert from the CA with role controller. The chain check is ours
// (RequireAnyClientCert) so the clock slack applies.
func ServerConfig(ca *x509.Certificate, own tls.Certificate, revoked func(serial string) bool) *tls.Config {
	return &tls.Config{
		MinVersion:            tls.VersionTLS13,
		Certificates:          []tls.Certificate{own},
		ClientAuth:            tls.RequireAnyClientCert,
		VerifyPeerCertificate: verifier(ca, RoleController, "", revoked),
	}
}

// ClientConfig is the controller's dial config: it presents own and verifies
// the worker cert against the CA. nodeID, when not empty, pins one node. The
// hostname check is off (nodes are dialled by address); identity is the
// cert's role and node id, checked by the verify hook.
func ClientConfig(ca *x509.Certificate, own tls.Certificate, nodeID string, revoked func(serial string) bool) *tls.Config {
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
// demands a worker cert from the CA.
func WorkerServerConfig(ca *x509.Certificate, own func() tls.Certificate, revoked func(serial string) bool) *tls.Config {
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
// presents own and verifies the controller cert against the CA.
func ControllerClientConfig(ca *x509.Certificate, own tls.Certificate) *tls.Config {
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

// SaveCA writes keys/cluster-ca.key (0600) and keys/cluster-ca.crt under dir.
func SaveCA(dir string, ca *CA) error {
	k, err := KeyPEM(ca.Key)
	if err != nil {
		return err
	}
	c := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: ca.Cert.Raw})
	if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(CAKeyFile)), 0o700); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, CAKeyFile), k, 0o600); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, CACertFile), c, 0o644)
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
