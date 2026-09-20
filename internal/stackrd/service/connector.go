package service

import (
	"context"

	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// ConnectorService owns the connector row: an organization's link to a source
// host, which is what a config-managed stack clones through and what a
// webhook arrives on.
//
// The install half — the GitHub App manifest flow, the token exchange — stays
// in infra/githubapp, which talks to GitHub. This owns the row that flow
// produces, so a page that lists or resolves a connector does not reach past
// it into the table.
type ConnectorService struct {
	store repo.Store
}

func NewConnectorService(store repo.Store) *ConnectorService {
	return &ConnectorService{store: store}
}

// Get is one connector by id.
func (s *ConnectorService) Get(ctx context.Context, id string) (*repo.Connector, error) {
	cn, err := s.store.GetConnector(ctx, id)
	if err != nil {
		return nil, err
	}
	if cn == nil {
		return nil, svcerr.ErrNotFound
	}
	return cn, nil
}

// ForOrg is an organization's connectors. This is the tenancy-scoped listing
// and the one a page wants: a connector carries credentials for somebody's
// source host, so showing one org's to another is the whole failure.
func (s *ConnectorService) ForOrg(ctx context.Context, orgID string) ([]repo.Connector, error) {
	return s.store.ListConnectorsByOrg(ctx, orgID)
}

// ListAll is every connector on the server. Its one caller is the webhook
// router, which has a delivery in hand and no org yet — it is matching an
// installation id, not showing anybody a list.
func (s *ConnectorService) ListAll(ctx context.Context) ([]repo.Connector, error) {
	return s.store.ListConnectors(ctx)
}

// Delete removes a connector.
func (s *ConnectorService) Delete(ctx context.Context, id string) error {
	return s.store.DeleteConnector(ctx, id)
}
