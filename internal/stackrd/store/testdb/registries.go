package testdb

import (
	"context"

	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// Registries satisfies infra/registry.Registries by writing and reading the
// store directly.
//
// The real implementation is service.RegistryService. infra/registry, and
// every package built on it, are packages service/ is built on, so their
// in-package tests cannot name it. Same shape, and same caveat, as WorkItems
// and NodeRows: the methods it doubles carry no rule today, and if one of
// them grows one these tests should fail rather than quietly keep passing.
type Registries struct{ Store repo.Store }

func (r Registries) ManagedOrNil(ctx context.Context) (*repo.Registry, error) {
	return r.Store.GetManagedRegistry(ctx)
}

func (r Registries) Credentials(ctx context.Context, orgID string) ([]repo.OrgRegistryCredential, error) {
	return r.Store.ListOrgRegistryCredentials(ctx, orgID)
}

func (r Registries) EnsureRow(ctx context.Context, reg *repo.Registry) error {
	return r.Store.CreateRegistry(ctx, reg)
}

func (r Registries) MintSystemCredential(ctx context.Context, c *repo.OrgRegistryCredential) error {
	return r.Store.CreateOrgRegistryCredential(ctx, c)
}

func (r Registries) RevokeSystemCredential(ctx context.Context, id string) error {
	return r.Store.DeleteSystemOrgRegistryCredential(ctx, id)
}
