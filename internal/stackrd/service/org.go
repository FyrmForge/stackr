package service

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// OrgService owns the organization row: reading one, and the draft an install
// is set up through.
//
// It is not a read shell over the orgs table. The two things that made it
// worth having are StartDraft — "one draft per person", which the panel got
// right and nothing else knew about — and UnfinishedDraft, whose two rules
// were written as comments inside a panel helper where no other surface could
// reach them.
type OrgService struct {
	store repo.Store
}

func NewOrgService(store repo.Store) *OrgService { return &OrgService{store: store} }

// DraftOrgName is what a draft organization is called until it is named,
// either by its config file or by the wizard's name step.
const DraftOrgName = "Untitled organization"

// Get is one organization by id, ErrNotFound rather than a nil row.
func (s *OrgService) Get(ctx context.Context, id string) (*repo.Org, error) {
	o, err := s.store.GetOrg(ctx, id)
	if err != nil {
		return nil, err
	}
	if o == nil {
		return nil, svcerr.ErrNotFound
	}
	return o, nil
}

// BySlug is one organization by its URL slug.
func (s *OrgService) BySlug(ctx context.Context, slug string) (*repo.Org, error) {
	o, err := s.store.GetOrgBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	if o == nil {
		return nil, svcerr.ErrNotFound
	}
	return o, nil
}

// ListForUser is the organizations this user belongs to. This is the
// tenancy-scoped listing: it is what a page shows somebody who is not a server
// admin, and it is never interchangeable with ListAll.
func (s *OrgService) ListForUser(ctx context.Context, userID string) ([]repo.Org, error) {
	return s.store.ListOrgsForUser(ctx, userID)
}

// ListAll is every organization on the server.
//
// Deliberately named apart from ListForUser. For tiles and stacks the
// unscoped listing is an admin convenience; for organizations the org IS the
// tenancy unit, so reaching for this one where ListForUser belongs does not
// widen a page, it crosses a tenant. Its callers are the admin branch of the
// API's org list, the server-wide setup-state map, and the "last organization
// cannot be deleted" count.
func (s *OrgService) ListAll(ctx context.Context) ([]repo.Org, error) {
	return s.store.ListOrgs(ctx)
}

// UnfinishedDraft is the organization this user is already halfway through
// setting up, if there is one. Nil is the normal answer.
//
// Two rules, both of which used to live only in a panel helper:
//
// The newest wins. ListOrgsForUser orders by name, so taking the first match
// would make "the draft" a store-order accident the moment someone owns two —
// an org they were invited to as owner and never finished, say.
//
// The caller's own role, not "is an admin". Any server admin passes an
// ownership test on any org, which would make a stranger's half-finished org
// the draft that this admin's next answer to step 1 moves.
func (s *OrgService) UnfinishedDraft(ctx context.Context, userID string) (*repo.Org, error) {
	orgs, err := s.store.ListOrgsForUser(ctx, userID)
	if err != nil {
		return nil, err
	}
	var newest *repo.Org
	for i := range orgs {
		if orgs[i].SetupDoneAt != nil {
			continue
		}
		m, err := s.store.GetOrgMember(ctx, orgs[i].ID, userID)
		if err != nil || m == nil || m.Role != "owner" {
			continue
		}
		if newest == nil || orgs[i].CreatedAt.After(newest.CreatedAt) {
			newest = &orgs[i]
		}
	}
	return newest, nil
}

// StartDraft answers step 1 of setup: it returns the organization the wizard
// should continue in, creating one if this user has no draft going.
//
// The org is created empty and named later, by whichever branch the owner
// picked: the config file names a managed org, the name step names a
// hand-built one. Asking for a name here and letting the file overwrite it
// seconds afterwards is what this replaced.
//
// Coming back to step 1 and picking the other branch is the same organization
// changing its mind, not a second one — matched on "unfinished and owned",
// not on the placeholder name, because the UI branch renames at its very
// first step.
//
// The creator becomes the org's owner. Who may reach this at all is the
// route's business, not this service's.
func (s *OrgService) StartDraft(ctx context.Context, userID, mode string) (*repo.Org, error) {
	if mode != "config" && mode != "ui" {
		return nil, invalid("mode", "pick how to set the organization up")
	}
	if o, err := s.UnfinishedDraft(ctx, userID); err != nil {
		return nil, err
	} else if o != nil {
		o.SetupMode = mode
		if err := s.store.UpdateOrg(ctx, o); err != nil {
			return nil, err
		}
		return o, nil
	}
	o := &repo.Org{
		ID:        uuid.New().String(),
		Name:      DraftOrgName,
		Slug:      draftSlug(),
		CreatedAt: time.Now().UTC(),
		SetupMode: mode,
	}
	if err := s.store.CreateOrg(ctx, o); err != nil {
		return nil, err
	}
	if err := s.store.UpsertOrgMember(ctx, &repo.OrgMember{
		OrgID: o.ID, UserID: userID, Role: "owner", CreatedAt: time.Now().UTC(),
	}); err != nil {
		return nil, err
	}
	return o, nil
}

// draftSlug is a URL for an org with no name yet. Random rather than counted:
// two people starting at once must not collide on the same slug.
func draftSlug() string { return "org-" + uuid.New().String()[:6] }

// Resolve is one organization addressed either way: by slug, falling back to
// its id.
//
// Slug first, id second, so that a slug that happens to look like an id
// cannot be shadowed. Both panel loaders (the canvas and the settings pages)
// had their own copy of this with a comment on each saying it had to match the
// other; the API's connector path had a third, written as a slug-to-id
// translation rather than a load.
func (s *OrgService) Resolve(ctx context.Context, key string) (*repo.Org, error) {
	o, err := s.store.GetOrgBySlug(ctx, key)
	if err != nil {
		return nil, err
	}
	if o == nil {
		if o, err = s.store.GetOrg(ctx, key); err != nil {
			return nil, err
		}
	}
	if o == nil {
		return nil, svcerr.ErrNotFound
	}
	return o, nil
}

// Save writes an organization row back. The rules that decide what may change
// belong to the callers that own them — the wizard's steps, the settings
// cascade — so this is the write itself, and no handler needs the store to
// make it.
func (s *OrgService) Save(ctx context.Context, o *repo.Org) error {
	return s.store.UpdateOrg(ctx, o)
}

// Delete removes an organization. What it cascades is the store's; whether it
// MAY be removed — the last organization on the install cannot be, and a draft
// does not count as one — is the panel's, because that rule is about what the
// setup wizard needs to leave behind.
func (s *OrgService) Delete(ctx context.Context, id string) error {
	return s.store.DeleteOrg(ctx, id)
}
