package service

import (
	"context"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/stackrd/config/secrets"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/registry"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// RegistryService owns the registry rows an admin manages, and the per-org
// push/pull credentials and image tags on the managed one.
//
// The row that matters is the delete. The panel's was
// `DeleteRegistry(c.Param("id"))` — no load, no check — where the API refuses
// the managed row with a 409. One POST removed the managed registry; boot
// recreates it with a fresh password, which breaks every org's derived system
// credential until the next EnsureSystemCredential, and in between every
// build pushes to a registry it cannot authenticate against.
//
// Underneath that, three smaller disagreements: the panel minted an org
// credential with no managed registry configured (the API answers 503), and
// the two in-use matchers behind "may this tag be deleted" disagreed — the
// panel's split the stored tag at its first slash and indexed by the tail, so
// a tag stored without a pull host matched nothing and was deletable while a
// live deployment still pointed at it.
type RegistryService struct {
	store repo.Store
}

func NewRegistryService(store repo.Store) *RegistryService {
	return &RegistryService{store: store}
}

// AddExternal records a registry stackr pulls from but does not run.
func (s *RegistryService) AddExternal(ctx context.Context, name, url, username, password string) (*repo.Registry, error) {
	r := &repo.Registry{
		ID: uuid.New().String(), Name: strings.TrimSpace(name), URL: strings.TrimSpace(url),
		Username: username, Password: password, CreatedAt: time.Now().UTC(),
	}
	if r.Name == "" || r.URL == "" {
		return nil, invalid("", "name and url required")
	}
	return r, s.store.CreateRegistry(ctx, r)
}

// Delete removes a registry row, never the managed one.
func (s *RegistryService) Delete(ctx context.Context, id string) error {
	r, err := s.store.GetRegistry(ctx, id)
	if err != nil || r == nil {
		return svcerr.ErrNotFound
	}
	if r.Managed {
		// Every build pushes to it, so a deploy would have nowhere to get its
		// image from — and boot would silently recreate it with a new
		// password, leaving every org's derived credential invalid. Clearing
		// its domain is the thing a caller can do.
		return svcerr.Conflictf("the managed registry cannot be removed")
	}
	return s.store.DeleteRegistry(ctx, r.ID)
}

// Managed is the registry stackr runs. Absent is unavailable, not missing:
// the caller's org is fine, the thing that is down is ours.
func (s *RegistryService) Managed(ctx context.Context) (*repo.Registry, error) {
	reg, err := s.store.GetManagedRegistry(ctx)
	if err != nil {
		return nil, err
	}
	if reg == nil {
		return nil, svcerr.ErrUnavailable
	}
	return reg, nil
}

// MintCredential creates an org push/pull credential and returns the secret,
// which exists only in this return value: the row stores a hash.
//
// A managed registry has to exist first. The panel minted without checking,
// producing a credential for a registry that was not there.
func (s *RegistryService) MintCredential(ctx context.Context, org *repo.Org, name string) (*repo.OrgRegistryCredential, string, error) {
	if org == nil {
		return nil, "", svcerr.ErrNotFound
	}
	if _, err := s.Managed(ctx); err != nil {
		return nil, "", err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, "", invalid("name", "required")
	}
	if name == registry.SystemCredentialName {
		return nil, "", svcerr.Conflictf("%q is the credential stackr's own deploys use; pick another name",
			registry.SystemCredentialName)
	}
	secret := secrets.RandomHex(24)
	cred := &repo.OrgRegistryCredential{
		ID: uuid.New().String(), OrgID: org.ID, Name: name,
		SecretHash: registry.HashSecret(secret), Prefix: secret[:8], CreatedAt: time.Now().UTC(),
	}
	if err := s.store.CreateOrgRegistryCredential(ctx, cred); err != nil {
		return nil, "", err
	}
	return cred, secret, nil
}

// RevokeCredential removes one, except the credential stackr's own deploys
// push with.
func (s *RegistryService) RevokeCredential(ctx context.Context, org *repo.Org, credID string) error {
	cred, err := s.store.GetOrgRegistryCredential(ctx, credID)
	if err != nil || cred == nil || org == nil || cred.OrgID != org.ID {
		return svcerr.ErrNotFound
	}
	if cred.System {
		return svcerr.Conflictf("this is the credential stackr's own deploys push with; removing it would break the next build")
	}
	return s.store.DeleteOrgRegistryCredential(ctx, cred.ID)
}

// Image resolves an image name inside the org's namespace. A short name is
// the friendly form the listing shows, a full one is what a script copies out
// of an image reference; both resolve, neither can reach outside.
func (s *RegistryService) Image(_ context.Context, org *repo.Org, name string) (string, error) {
	if org == nil {
		return "", svcerr.ErrNotFound
	}
	ns := registry.Namespace(org.Slug)
	if !strings.HasPrefix(name, ns) {
		name = ns + name
	}
	if !strings.HasPrefix(name, ns) || !registry.ValidRepoName(name) {
		return "", svcerr.ErrNotFound
	}
	return name, nil
}

// TagInUse reports where a tag is still deployed, "" when nothing points at
// it. One matcher, where the panel and the API had two that disagreed.
//
// The stored ImageTag usually carries the pull host and sometimes does not,
// so both spellings are matched. The panel's version split at the first slash
// and indexed by the tail, which silently skipped every host-less tag — those
// were deletable from the panel and protected over the API.
func (s *RegistryService) TagInUse(ctx context.Context, org *repo.Org, name, tag string) (string, error) {
	if org == nil {
		return "", svcerr.ErrNotFound
	}
	if !registry.ValidTag(tag) {
		return "", invalid("tag", "malformed tag")
	}
	want := name + ":" + tag
	stacks, err := s.store.ListStacksByOrg(ctx, org.ID)
	if err != nil {
		return "", err
	}
	for _, st := range stacks {
		tiles, err := s.store.ListTilesByStack(ctx, st.ID)
		if err != nil {
			continue
		}
		for i := range tiles {
			ds, err := s.store.ListDeploymentsByTile(ctx, tiles[i].ID, 5)
			if err != nil {
				continue
			}
			for _, d := range ds {
				if d.Status != "done" || d.ImageTag == "" {
					continue
				}
				if d.ImageTag == want || strings.HasSuffix(d.ImageTag, "/"+want) {
					return st.Slug + "/" + tiles[i].Slug, nil
				}
			}
		}
	}
	return "", nil
}

// --- reads ---

// Get is one registry by id.
func (s *RegistryService) Get(ctx context.Context, id string) (*repo.Registry, error) {
	r, err := s.store.GetRegistry(ctx, id)
	if err != nil {
		return nil, err
	}
	if r == nil {
		return nil, svcerr.ErrNotFound
	}
	return r, nil
}

// ListAll is every registry this install can pull from or push to.
func (s *RegistryService) ListAll(ctx context.Context) ([]repo.Registry, error) {
	return s.store.ListRegistries(ctx)
}

// Credentials are an organization's push/pull credentials for the managed
// registry. The secret half is not here: only the row.
func (s *RegistryService) Credentials(ctx context.Context, orgID string) ([]repo.OrgRegistryCredential, error) {
	return s.store.ListOrgRegistryCredentials(ctx, orgID)
}

// Create adds a registry row. AddExternal is the one with the rules — it
// validates the URL and the credentials; this is the raw write for the
// managed registry, which stackr creates for itself.
func (s *RegistryService) Create(ctx context.Context, r *repo.Registry) error {
	return s.store.CreateRegistry(ctx, r)
}

// Save writes a registry row back.
func (s *RegistryService) Save(ctx context.Context, r *repo.Registry) error {
	return s.store.UpdateRegistry(ctx, r)
}
