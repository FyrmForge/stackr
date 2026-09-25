package service

import (
	"context"

	"github.com/FyrmForge/stackr/internal/service/internal/leaf/domainres"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// DomainResource is a host stackr names tiles under, at instance, org or
// stack level (REWRITE.md "Domain resources").
type DomainResource = store.DomainResource

// CreateDomainResource adds a declared resource. level is instance, org or
// stack; ownerID the org or stack id ("" at instance level). The squat
// check counts the host against the owner's org. A resource with its own
// ACME email re-pushes the proxy, where the account lives.
func (o *Orchestrator) CreateDomainResource(
	ctx context.Context,
	level, ownerID, host string,
	includeEnvOnDefault bool,
	acmeEmail string,
) (DomainResource, error) {
	ownOrgID := ""
	switch level {
	case domainres.Org:
		og, err := o.orgs.Get(ctx, ownerID)
		if err != nil {
			return DomainResource{}, err
		}
		ownOrgID = og.ID
	case domainres.Stack:
		st, err := o.stacks.Get(ctx, ownerID)
		if err != nil {
			return DomainResource{}, err
		}
		ownOrgID = st.OrgID
	}
	orgs, err := o.orgs.ListAll(ctx)
	if err != nil {
		return DomainResource{}, err
	}
	spec := domainres.Spec{
		Level:               level,
		OwnerID:             ownerID,
		Host:                host,
		IncludeEnvOnDefault: includeEnvOnDefault,
		ACMEEmail:           acmeEmail,
	}
	r, err := o.domainres.Create(ctx, spec, ownOrgID, orgs)
	if err != nil || r.ACMEEmail == "" {
		return r, err
	}
	return r, o.sync.Sync(ctx)
}

// UpdateDomainResource sets the env flag and the ACME email; a moved email
// re-pushes the proxy. Auto names already written keep their host until
// their stack's next promote.
func (o *Orchestrator) UpdateDomainResource(
	ctx context.Context,
	id string,
	includeEnvOnDefault bool,
	acmeEmail string,
) (DomainResource, error) {
	r, err := o.domainres.Get(ctx, id)
	if err != nil {
		return r, err
	}
	was := r.ACMEEmail
	r, err = o.domainres.Update(ctx, r, includeEnvOnDefault, acmeEmail)
	if err != nil || r.ACMEEmail == was {
		return r, err
	}
	return r, o.sync.Sync(ctx)
}

// DeleteDomainResource is refused while tile domains carry its name.
func (o *Orchestrator) DeleteDomainResource(ctx context.Context, id string) error {
	r, err := o.domainres.Get(ctx, id)
	if err != nil {
		return err
	}
	ds, err := o.domains.List(ctx)
	if err != nil {
		return err
	}
	named := 0
	for _, d := range ds {
		if d.ResourceID != nil && *d.ResourceID == r.ID {
			named++
		}
	}
	err = o.domainres.Delete(ctx, r, named)
	if err != nil || r.ACMEEmail == "" {
		return err
	}
	return o.sync.Sync(ctx)
}

// DomainResources is the org drawer's list: the org's own rows, then the
// instance's, nearest first. Stack rows belong to their stack file.
func (o *Orchestrator) DomainResources(ctx context.Context, orgID string) ([]DomainResource, error) {
	return o.domainres.Visible(ctx, "", orgID)
}

// AllDomainResources is every resource on the server (admin).
func (o *Orchestrator) AllDomainResources(ctx context.Context) ([]DomainResource, error) {
	return o.domainres.ListAll(ctx)
}
