package registry

// Token auth for the managed registry.
//
// The registry used to run on one htpasswd user for the whole cluster, and
// images sit at <org>_<stack>_<tile>. Any org's tile could name another org's
// image and the logged-in daemon would pull it: the namespace was a naming
// convention, not a boundary.
//
// Now the registry runs REGISTRY_AUTH=token and points its realm at stackrd.
// stackrd checks an org registry credential and mints a short-lived JWT scoped
// to repository:<org-slug>_*, so the boundary is the registry's own, enforced
// on every request it serves rather than on whoever happened to be logged in
// on the node.
//
// No JWT dependency: the token is three base64 segments and an RS256 signature,
// and the header carries the signing certificate in x5c, which is the
// verification path the registry supports without also matching libtrust key
// ids.

import (
	"context"
	"crypto"
	"crypto/hmac"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Issuer and Service name this stackr in the tokens it mints. They must match
// the registry's REGISTRY_AUTH_TOKEN_ISSUER and _SERVICE or every token is
// refused with no useful message.
const (
	Issuer  = "stackr"
	Service = "stackr-registry"
	// TokenTTL is short on purpose: the credential is re-presented per pull and
	// push, so nothing needs a long-lived token, and a leaked one expires
	// before it is useful.
	TokenTTL = 5 * time.Minute
	// TokenPath is the route stackrd serves the exchange on, appended to the
	// panel's base URL to make the realm the registry advertises.
	TokenPath = "/v2/token"
)

// Signer mints registry tokens. One per process, loaded from (or created in)
// the data directory so a restart does not invalidate the registry's root
// bundle.
type Signer struct {
	key     *rsa.PrivateKey
	certDER []byte
}

// LoadSigner reads the token keypair from dir, creating a self-signed one on
// first run. The certificate is what the registry container mounts as its
// rootcertbundle, so the two halves have to come from the same place.
func LoadSigner(dir string) (*Signer, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	keyPath, certPath := filepath.Join(dir, "token.key"), filepath.Join(dir, "token.crt")
	keyPEM, keyErr := os.ReadFile(keyPath)
	certPEM, certErr := os.ReadFile(certPath)
	if keyErr == nil && certErr == nil {
		s, err := parseSigner(keyPEM, certPEM)
		if err == nil {
			return s, nil
		}
		// A half-written or corrupt pair is regenerated rather than fatal: the
		// registry reloads the bundle when the service rolls, and refusing to
		// boot would leave the panel down over a file nothing else reads.
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return nil, err
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: Issuer},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	if err := os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{
		Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}), 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{
		Type: "CERTIFICATE", Bytes: der}), 0o644); err != nil {
		return nil, err
	}
	return &Signer{key: key, certDER: der}, nil
}

func parseSigner(keyPEM, certPEM []byte) (*Signer, error) {
	kb, _ := pem.Decode(keyPEM)
	cb, _ := pem.Decode(certPEM)
	if kb == nil || cb == nil {
		return nil, fmt.Errorf("registry token keypair is not PEM")
	}
	key, err := x509.ParsePKCS1PrivateKey(kb.Bytes)
	if err != nil {
		return nil, err
	}
	if _, err := x509.ParseCertificate(cb.Bytes); err != nil {
		return nil, err
	}
	return &Signer{key: key, certDER: cb.Bytes}, nil
}

// Access is one repository the token grants, in the registry's own shape.
type Access struct {
	Type    string   `json:"type"`
	Name    string   `json:"name"`
	Actions []string `json:"actions"`
}

// Sign mints a token for subject with the given access set. An empty access set
// is still a valid token: that is how the registry's own /v2/ ping succeeds for
// a caller who has authenticated but asked for nothing.
func (s *Signer) Sign(subject string, access []Access) (string, time.Time, error) {
	now := time.Now()
	exp := now.Add(TokenTTL)
	if access == nil {
		access = []Access{}
	}
	header := map[string]any{
		"typ": "JWT",
		"alg": "RS256",
		// The certificate itself rather than a key id: the registry verifies
		// the chain against its rootcertbundle, and this avoids having to
		// reproduce libtrust's key-id derivation exactly.
		"x5c": []string{base64.StdEncoding.EncodeToString(s.certDER)},
	}
	claims := map[string]any{
		"iss":    Issuer,
		"sub":    subject,
		"aud":    Service,
		"exp":    exp.Unix(),
		"nbf":    now.Add(-time.Minute).Unix(), // clock skew between host and container
		"iat":    now.Unix(),
		"jti":    randomID(),
		"access": access,
	}
	hb, err := json.Marshal(header)
	if err != nil {
		return "", exp, err
	}
	cb, err := json.Marshal(claims)
	if err != nil {
		return "", exp, err
	}
	signing := b64(hb) + "." + b64(cb)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, sum[:])
	if err != nil {
		return "", exp, err
	}
	return signing + "." + b64(sig), exp, nil
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// Namespace is the repository prefix one org owns: everything under it is
// theirs, and nothing outside it is reachable with their token.
//
// Repositories are flat and underscore-joined: envnet.Scope.ImageRepo is
// stkr/<org>_<stack>_<tile> and deploy strips the stkr/ push prefix, so what
// the registry stores is <org>_<stack>_<tile>. Slugs are [a-z0-9-] only, so
// the trailing underscore is an unambiguous boundary: acme_ cannot match
// acme-evil_.
func Namespace(orgSlug string) string {
	return orgSlug + "_"
}

// GrantAll hands back every scope the client asked for. Only the admin root
// credential gets this: the agent image and the garbage collector live outside
// any org's namespace, so a token scoped to one org cannot reach them.
func GrantAll(scopes []string) []Access {
	var out []Access
	for _, raw := range scopes {
		for _, one := range strings.Split(raw, " ") {
			parts := strings.Split(one, ":")
			if len(parts) < 3 || parts[0] != "repository" {
				continue
			}
			out = append(out, Access{Type: "repository",
				Name:    strings.Join(parts[1:len(parts)-1], ":"),
				Actions: strings.Split(parts[len(parts)-1], ",")})
		}
	}
	return out
}

// GrantFor narrows a requested scope to what the org may actually have. The
// registry sends what the client asked for; answering with a token that
// contains it unchanged is exactly the hole this replaces.
//
// scopes are the raw `scope` query values, each
// "repository:<name>:<action>[,<action>]". Anything outside the org's namespace
// is dropped rather than refused: the registry turns a missing grant into its
// own 401, which is the message a docker client knows how to render.
func GrantFor(orgSlug string, scopes []string) []Access {
	ns := Namespace(orgSlug)
	var out []Access
	for _, raw := range scopes {
		for _, one := range strings.Split(raw, " ") {
			parts := strings.Split(one, ":")
			if len(parts) < 3 || parts[0] != "repository" {
				continue
			}
			// A repository name may itself contain colons only in the tag,
			// which is not part of a scope, so the middle is everything but
			// the first and last field.
			name := strings.Join(parts[1:len(parts)-1], ":")
			if !strings.HasPrefix(name, ns) {
				continue
			}
			out = append(out, Access{Type: "repository", Name: name,
				Actions: strings.Split(parts[len(parts)-1], ",")})
		}
	}
	return out
}

// --- credentials ---

// SystemCredentialName is the credential stackr's own deploys push with. One
// per org, created on demand, not deletable: without it the org's next build
// has nothing to push with.
const SystemCredentialName = "stackr"

// systemSecret derives the system credential's plaintext from the managed
// registry's own password and the org id.
//
// Derived rather than stored: the table keeps only hashes, so that a database
// copy is not a set of push credentials, and stackr still needs the plaintext
// to put in a pull/push auth header. Rotating the registry password rotates
// every org's system credential, which EnsureSystemCredential then re-records.
func systemSecret(registryPassword, orgID string) string {
	mac := hmac.New(sha256.New, []byte(registryPassword))
	_, _ = mac.Write([]byte("system-registry-credential:" + orgID))
	return hex.EncodeToString(mac.Sum(nil))
}

// HashSecret is how a credential's plaintext is stored and looked up. Same
// shape as an API key hash, deliberately: one rule for "a bearer string in this
// database is a hash".
func HashSecret(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// EnsureSystemCredential returns the org's stackr-owned push secret, creating
// or repairing its row as needed.
func EnsureSystemCredential(ctx context.Context, store repo.Store, regs Registries, reg *repo.Registry, org *repo.Org) (string, error) {
	if reg == nil || org == nil {
		return "", fmt.Errorf("no registry or organization")
	}
	secret := systemSecret(reg.Password, org.ID)
	hash := HashSecret(secret)
	creds, err := store.ListOrgRegistryCredentials(ctx, org.ID)
	if err != nil {
		return "", err
	}
	for i := range creds {
		if !creds[i].System {
			continue
		}
		if creds[i].SecretHash == hash {
			return secret, nil
		}
		// The registry password changed under it: the old hash can never match
		// again, so the row is replaced rather than left as a dead credential.
		if err := regs.RevokeSystemCredential(ctx, creds[i].ID); err != nil {
			return "", err
		}
		break
	}
	if err := regs.MintSystemCredential(ctx, &repo.OrgRegistryCredential{
		ID: uuid.New().String(), OrgID: org.ID, Name: SystemCredentialName,
		SecretHash: hash, Prefix: secret[:8], System: true, CreatedAt: time.Now().UTC(),
	}); err != nil {
		return "", err
	}
	return secret, nil
}

// AgentUser and AgentRepo are the pull-only identity the node agent's service
// spec carries. The agent image lives outside every org's namespace, so it
// used to travel with the registry's root pair: a credential good for every
// org's images, on every node, permanently, because the agent is a global
// service.
const (
	AgentUser = "stkr-agent"
	AgentRepo = "stkr-agent"
)

// AgentSecret derives the agent's pull secret from the registry password, the
// same shape as systemSecret and for the same reason: nothing to store, and
// rotating the registry password rotates it.
func AgentSecret(registryPassword string) string {
	mac := hmac.New(sha256.New, []byte(registryPassword))
	_, _ = mac.Write([]byte("agent-registry-credential"))
	return hex.EncodeToString(mac.Sum(nil))
}

// GrantAgentPull narrows what the agent identity may have to pulling its own
// image. Asking for anything else, including a push of it, gets nothing.
func GrantAgentPull(scopes []string) []Access {
	for _, a := range GrantAll(scopes) {
		if a.Name != AgentRepo {
			continue
		}
		for _, act := range a.Actions {
			if act == "pull" {
				return []Access{{Type: "repository", Name: AgentRepo, Actions: []string{"pull"}}}
			}
		}
	}
	return nil
}

// OrgCredential resolves the org that owns a tile and its stackr-owned push
// secret, with the managed registry row. One place rather than per caller: a
// caller that reaches for the registry's root pair instead hands that node a
// credential good for every org's images.
func OrgCredential(ctx context.Context, store repo.Store, regs Registries, t *repo.Tile) (*repo.Registry, *repo.Org, string, error) {
	reg, err := store.GetManagedRegistry(ctx)
	if err != nil {
		return nil, nil, "", err
	}
	if reg == nil {
		return nil, nil, "", fmt.Errorf("no registry configured")
	}
	stack, err := store.GetStack(ctx, t.StackID)
	if err != nil || stack == nil {
		return nil, nil, "", fmt.Errorf("stack for %s: %w", t.Slug, errOr(err, "not found"))
	}
	org, err := store.GetOrg(ctx, stack.OrgID)
	if err != nil || org == nil {
		return nil, nil, "", fmt.Errorf("organization for %s: %w", t.Slug, errOr(err, "not found"))
	}
	secret, err := EnsureSystemCredential(ctx, store, regs, reg, org)
	if err != nil {
		return nil, nil, "", err
	}
	return reg, org, secret, nil
}

// OrgPullAuth is the pull address and the org-scoped auth blob for a tile's
// image, which is what swarm hands the node that has to pull it.
func OrgPullAuth(ctx context.Context, store repo.Store, regs Registries, rt *runtime.Runtime, t *repo.Tile) (string, string, error) {
	reg, org, secret, err := OrgCredential(ctx, store, regs, t)
	if err != nil {
		return "", "", err
	}
	host, err := PullAddr(ctx, rt, reg)
	if err != nil {
		return "", "", err
	}
	return host, Auth(org.Slug, secret, host), nil
}

func errOr(err error, msg string) error {
	if err != nil {
		return err
	}
	return errors.New(msg)
}

// Auth builds the base64 blob the Docker API takes in X-Registry-Auth, and
// swarm passes on to every node so a worker can pull the image itself. Without
// it a task placed on a worker pulls anonymously and the registry refuses it.
//
// host is the address the caller dials: a push uses the manager's own
// localhost, a pull uses the address every node can reach.
func Auth(orgSlug, secret, host string) string {
	if orgSlug == "" || secret == "" {
		return ""
	}
	b, err := json.Marshal(map[string]string{
		"username":      orgSlug,
		"password":      secret,
		"serveraddress": host,
	})
	if err != nil {
		return ""
	}
	return base64.URLEncoding.EncodeToString(b)
}

// OrgHasImages reports whether anything has ever been pushed under the org's
// registry namespace.
//
// Read from the deployments table rather than the registry's catalog API: the
// namespace is the org slug and the registry has no rename, so a rename would
// have to re-tag every image (a blob mount plus a manifest push per tag) or
// orphan them. Refusing the rename is the smaller thing to be right about, and
// refusing it needs an answer even while the registry is down.
//
// walks the org's stacks and tiles. Tens of rows; add a store query
// if an org ever grows big enough to notice.
func OrgHasImages(ctx context.Context, store repo.Store, orgID string) (bool, error) {
	stacks, err := store.ListStacksByOrg(ctx, orgID)
	if err != nil {
		return false, err
	}
	for _, st := range stacks {
		tiles, err := store.ListTilesByStack(ctx, st.ID)
		if err != nil {
			return false, err
		}
		for i := range tiles {
			ds, err := store.ListDeploymentsByTile(ctx, tiles[i].ID, 1)
			if err != nil {
				return false, err
			}
			for _, d := range ds {
				if d.ImageTag != "" {
					return true, nil
				}
			}
		}
	}
	return false, nil
}
