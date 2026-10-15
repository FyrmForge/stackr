// Package jointoken owns join_tokens: the single use, short lived secrets an
// admin hands to a new worker, and what that worker sends back with one (CSR,
// WireGuard key, facts) until an admin confirms or rejects it. Only a hash of
// a token is stored.
package jointoken

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base32"
	"encoding/hex"
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Security log actions of the join flow.
const (
	ActionToken   = "join.token"
	ActionRequest = "join.request"
	ActionConfirm = "node.confirm"
	ActionReject  = "node.reject"
	ActionCert    = "cert.issue"
)

// Prefix marks a join token, so a leaked one is recognisable.
const Prefix = "stkjoin_"

type Leaf struct{ rows store.JoinTokenStore }

func New(rows store.JoinTokenStore) *Leaf { return &Leaf{rows} }

// Hash is what is stored for a token.
func Hash(token string) string {
	h := sha256.Sum256([]byte(token))
	return hex.EncodeToString(h[:])
}

// Mint stores a new token for a node name, tags and the orgs the node opens
// to (openAll, else orgIDs) and returns the token, which exists nowhere else.
func (l *Leaf) Mint(ctx context.Context, name string, tags []string, openAll bool, orgIDs []string, by string, ttl time.Duration) (string, store.JoinToken, error) {
	b := make([]byte, 20)
	if _, err := rand.Read(b); err != nil {
		return "", store.JoinToken{}, err
	}
	token := Prefix + strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(b))
	t, _ := json.Marshal(tags)
	if orgIDs == nil {
		orgIDs = []string{}
	}
	o, _ := json.Marshal(orgIDs)
	now := time.Now().UTC()
	row := store.JoinToken{
		ID: uuid.NewString(), Hash: Hash(token), NodeName: name, Tags: string(t),
		CreatedBy: by, CreatedAt: now, ExpiresAt: now.Add(ttl), OpenToAll: openAll, OrgIDs: string(o),
	}
	return token, row, l.rows.Create(ctx, row)
}

// Spend uses the token up and returns its row. An unknown, expired or used
// token is one answer (ErrNotFound), so a probe learns nothing.
func (l *Leaf) Spend(ctx context.Context, token string) (store.JoinToken, error) {
	h := Hash(token)
	if err := l.rows.Spend(ctx, h, time.Now().UTC()); err != nil {
		return store.JoinToken{}, err
	}
	return l.rows.GetByHash(ctx, h)
}

// Live is the row of a token that could still be spent: well formed, unused,
// unexpired, never rejected. Anything else is ErrNotFound, one answer for
// every miss. It spends nothing (the join script and its download).
func (l *Leaf) Live(ctx context.Context, token string) (store.JoinToken, error) {
	if !tokenRE.MatchString(token) {
		return store.JoinToken{}, errs.ErrNotFound
	}
	t, err := l.rows.GetByHash(ctx, Hash(token))
	if err != nil {
		return store.JoinToken{}, err
	}
	if t.UsedAt != nil || t.RejectedAt != nil || !time.Now().UTC().Before(t.ExpiresAt) {
		return store.JoinToken{}, errs.ErrNotFound
	}
	return t, nil
}

// tokenRE is what Mint makes: the prefix and 20 random bytes in base32.
var tokenRE = regexp.MustCompile(`^` + Prefix + `[a-z2-7]{32}$`)

// Record keeps what the joining node sent.
func (l *Leaf) Record(ctx context.Context, t store.JoinToken, nodeID, csr, wgKey, facts string) error {
	t.NodeID, t.CSR, t.WGPublicKey, t.Facts = nodeID, csr, wgKey, facts
	return l.rows.Update(ctx, t)
}

// ByToken is the row of a spent token (the worker polling its status). A
// token never spent is not found: there is nothing to poll yet.
func (l *Leaf) ByToken(ctx context.Context, token string) (store.JoinToken, error) {
	t, err := l.rows.GetByHash(ctx, Hash(token))
	if err == nil && t.UsedAt == nil {
		return store.JoinToken{}, errs.ErrNotFound
	}
	return t, err
}

func (l *Leaf) ByNode(ctx context.Context, nodeID string) (store.JoinToken, error) {
	return l.rows.GetByNode(ctx, nodeID)
}

// SetCert keeps the issued cert for the worker to fetch.
func (l *Leaf) SetCert(ctx context.Context, t store.JoinToken, certPEM string) error {
	t.CertPEM = certPEM
	return l.rows.Update(ctx, t)
}

func (l *Leaf) SetRejected(ctx context.Context, t store.JoinToken, at time.Time) error {
	t.RejectedAt = &at
	return l.rows.Update(ctx, t)
}

// Used is every spent token, oldest first.
func (l *Leaf) Used(ctx context.Context) ([]store.JoinToken, error) { return l.rows.Used(ctx) }
