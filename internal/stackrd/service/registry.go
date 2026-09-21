package service

import (
	"context"
	"crypto/subtle"
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

// ManagedOrNil is Managed for a caller deciding whether to do something at
// all, rather than one answering a request: boot, which creates the row when
// it finds none, and the render and deploy paths, for which "no managed
// registry on this install" is an ordinary state and not a refusal to report.
//
// Managed's ErrUnavailable is right for a handler and wrong for those: it
// would turn the commonest configuration into an error branch.
func (s *RegistryService) ManagedOrNil(ctx context.Context) (*repo.Registry, error) {
	return s.store.GetManagedRegistry(ctx)
}

// RegistryLogin is who a registry client turned out to be, and what its token
// may carry.
type RegistryLogin struct {
	// Subject is what goes in the token: the registry's own user, the agent
	// user, or "<org slug>/<credential name>".
	Subject string
	Access  []registry.Access
}

// Authenticate identifies a docker client from the basic-auth pair it
// presented and returns the access its token may carry, already narrowed to
// the scopes it asked for.
//
// A nil login with a nil error is "that pair matches nothing" — the caller
// answers 401. It is not an svcerr refusal because this endpoint is outside
// the edge mapping on purpose: a docker client needs a WWW-Authenticate realm
// on the way out, which is the handler's to write and not a status code's.
//
// The three identities in order, and why the order matters:
//
//  1. The managed registry's own user. The agent image and the garbage
//     collector live outside every org's namespace, so a token scoped to one
//     org cannot reach them.
//  2. The agent's pull-only identity, derived from the registry password
//     rather than stored, so rotating the password rotates it and there is no
//     row to keep. It reaches the agent image and nothing else.
//  3. An org credential. The username is the org slug, so a real secret
//     presented against the wrong org is refused — without that check the
//     slug in the request would be decoration.
//
// The access is always narrowed and never echoed back. Handing the client the
// scope it asked for is exactly the cross-tenant hole this replaces.
func (s *RegistryService) Authenticate(ctx context.Context, user, secret string, scopes []string) (*RegistryLogin, error) {
	reg, err := s.store.GetManagedRegistry(ctx)
	if err != nil {
		return nil, err
	}
	if reg != nil && reg.Username != "" && user == reg.Username &&
		subtle.ConstantTimeCompare([]byte(secret), []byte(reg.Password)) == 1 {
		return &RegistryLogin{Subject: user, Access: registry.GrantAll(scopes)}, nil
	}
	if reg != nil && reg.Password != "" && user == registry.AgentUser &&
		subtle.ConstantTimeCompare([]byte(secret), []byte(registry.AgentSecret(reg.Password))) == 1 {
		return &RegistryLogin{Subject: user, Access: registry.GrantAgentPull(scopes)}, nil
	}

	cred, err := s.store.GetOrgRegistryCredentialByHash(ctx, registry.HashSecret(secret))
	if err != nil {
		return nil, err
	}
	org, err := s.store.GetOrgBySlug(ctx, user)
	if err != nil {
		return nil, err
	}
	if cred == nil || org == nil || cred.OrgID != org.ID {
		return nil, nil
	}
	if err := s.TouchCredential(ctx, cred.ID); err != nil {
		return nil, err
	}
	return &RegistryLogin{
		Subject: org.Slug + "/" + cred.Name,
		Access:  registry.GrantFor(org.Slug, scopes),
	}, nil
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

// --- the rows infra/registry writes while keeping itself running ---

// EnsureRow stores the managed registry's own row on first boot.
func (s *RegistryService) EnsureRow(ctx context.Context, r *repo.Registry) error {
	return s.store.CreateRegistry(ctx, r)
}

// MintSystemCredential stores a credential the panel issues to itself, for
// pulling its own images. Not MintCredential: that one is a person asking,
// and carries the org rules.
func (s *RegistryService) MintSystemCredential(ctx context.Context, c *repo.OrgRegistryCredential) error {
	return s.store.CreateOrgRegistryCredential(ctx, c)
}

// RevokeSystemCredential drops a system credential being rotated out.
func (s *RegistryService) RevokeSystemCredential(ctx context.Context, id string) error {
	return s.store.DeleteSystemOrgRegistryCredential(ctx, id)
}

// TouchCredential stamps last-used on a credential that just authenticated.
func (s *RegistryService) TouchCredential(ctx context.Context, id string) error {
	return s.store.TouchOrgRegistryCredential(ctx, id)
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
