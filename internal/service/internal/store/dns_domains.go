package store

import (
	"context"
	"time"
)

// DNSDomain is a row of dns_domains: a domain of a DNS connector's account
// that stackr manages. OrgID is the connector's org, nil for a server
// connector, whose domain is available to every org (ShareAll) or OrgIDs.
type DNSDomain struct {
	ID          string     `db:"id" json:"id"`
	ConnectorID string     `db:"connector_id" json:"connector_id"`
	OrgID       *string    `db:"org_id" json:"org_id"`
	Name        string     `db:"name" json:"name"`
	ShareAll    bool       `db:"share_all" json:"share_all"`
	OrgIDs      StringList `db:"org_ids" json:"org_ids"`
	CreatedAt   time.Time  `db:"created_at" json:"created_at"`
}

type DNSDomainStore interface {
	Create(ctx context.Context, d DNSDomain) error
	Update(ctx context.Context, d DNSDomain) error
	Delete(ctx context.Context, id string) error
	ListByConnector(ctx context.Context, connectorID string) ([]DNSDomain, error)
	// ListFor is the domains orgID may use: its own connectors', and the
	// server connectors' shared with it.
	ListFor(ctx context.Context, orgID string) ([]DNSDomain, error)
}

var dnsDomainsT = newTable[DNSDomain]("dns_domains", nil)

type dnsDomains struct{ crud[DNSDomain] }

func (s dnsDomains) ListByConnector(ctx context.Context, connectorID string) ([]DNSDomain, error) {
	return s.many(ctx, "connector_id = ? ORDER BY name", connectorID)
}

func (s dnsDomains) ListFor(ctx context.Context, orgID string) ([]DNSDomain, error) {
	return s.many(ctx, `org_id = ? OR (org_id IS NULL AND (share_all = 1 OR EXISTS
		(SELECT 1 FROM json_each(org_ids) WHERE value = ?))) ORDER BY name`, orgID, orgID)
}
