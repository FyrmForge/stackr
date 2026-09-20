package service

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// APIKeyService mints and revokes API keys.
//
// The token format and the row were written out three times — the account
// page, the CLI login's grant step and the CLI's unauthenticated exchange —
// so "what a key looks like" was a fact three files had to agree on.
type APIKeyService struct {
	store repo.Store
}

func NewAPIKeyService(store repo.Store) *APIKeyService {
	return &APIKeyService{store: store}
}

// HashAPIKey is the stored form of a raw token. Exported because the
// authenticator hashes the header to look a key up, and it must be the same
// function that wrote it.
func HashAPIKey(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

// Mint creates a key and returns the raw token, which exists only in this
// return value: the row stores a hash, so there is no second chance to read
// it.
//
// scopes are already filtered to what the minter may grant — that filter
// belongs with the scope catalog, which lives in the API package and cannot
// be imported from here. What is here is everything that made the three
// copies copies: the token prefix, the hash, the row and the empty check.
//
// orgID is the org whose role justified those scopes, and binds the key to it.
// Empty means unbound, which is the pre-existing behaviour: the caller passes
// "" for a server admin (granted by the admin badge, not by an org) and when
// there is no active org at all. See repo.APIKey.
func (s *APIKeyService) Mint(ctx context.Context, userID, orgID, name string, scopes []string) (*repo.APIKey, string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, "", invalid("name", "required")
	}
	if userID == "" {
		return nil, "", svcerr.ErrForbidden
	}
	if len(scopes) == 0 {
		return nil, "", invalid("scopes", "select at least one permission")
	}
	blob, err := json.Marshal(scopes)
	if err != nil {
		return nil, "", err
	}
	raw := "sk_" + uuid.New().String()
	k := &repo.APIKey{
		ID:        uuid.New().String(),
		UserID:    userID,
		Name:      name,
		TokenHash: HashAPIKey(raw),
		Scopes:    string(blob),
		OrgID:     sql.NullString{String: orgID, Valid: orgID != ""},
		CreatedAt: time.Now().UTC(),
	}
	if err := s.store.CreateAPIKey(ctx, k); err != nil {
		return nil, "", err
	}
	return k, raw, nil
}

// ListAll is every API key on the server, secrets excluded — the store never
// returns the hash. The account page filters to the caller's own.
func (s *APIKeyService) ListAll(ctx context.Context) ([]repo.APIKey, error) {
	return s.store.ListAPIKeys(ctx)
}
