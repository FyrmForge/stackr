package service

import (
	"context"

	"github.com/FyrmForge/stackr/internal/installspec"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/registry"
)

// OrgRepositories lists the org's repositories from the local registry, by
// name. The registry being down is an error the page shows as its text.
func (o *Orchestrator) OrgRepositories(ctx context.Context, orgID string) ([]registry.Repository, error) {
	og, err := o.store.Orgs.Get(ctx, orgID)
	if err != nil {
		return nil, err
	}
	return o.regs.List(ctx, og.Slug)
}

// RegistryHost is the address the registry answers at ("" until the panel
// has a domain).
func (o *Orchestrator) RegistryHost(ctx context.Context) string {
	panel, _ := o.settings.Get(ctx, "panel_domain")
	if panel == "" {
		return ""
	}
	return RegistryHostPrefix + installspec.CleanHost(panel)
}
