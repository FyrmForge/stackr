package store

import (
	"context"
	"time"
)

// Credential is a row of credentials: registry pull creds for one org.
type Credential struct {
	ID        string    `db:"id"`
	OrgID     string    `db:"org_id"`
	Name      string    `db:"name"`
	URL       string    `db:"url"`
	Username  string    `db:"username"`
	Password  string    `db:"password"`
	CreatedAt time.Time `db:"created_at"`
}

type CredentialStore interface {
	Create(ctx context.Context, c Credential) error
	Get(ctx context.Context, id string) (Credential, error)
	ListByOrg(ctx context.Context, orgID string) ([]Credential, error)
	Update(ctx context.Context, c Credential) error
	Delete(ctx context.Context, id string) error
}

var credentialsT = newTable("credentials", func(c *Credential) []*string { return []*string{&c.Password} })

type credentials struct{ crud[Credential] }

func (s credentials) ListByOrg(ctx context.Context, orgID string) ([]Credential, error) {
	return s.many(ctx, "org_id = ?", orgID)
}

// Connector is a row of connectors: one git App per org and host.
type Connector struct {
	ID        string    `db:"id"`
	OrgID     string    `db:"org_id"`
	Provider  string    `db:"provider"`
	Name      string    `db:"name"`
	Host      string    `db:"host"`
	Config    string    `db:"config"`
	CreatedAt time.Time `db:"created_at"`
}

type ConnectorStore interface {
	Create(ctx context.Context, c Connector) error
	Get(ctx context.Context, id string) (Connector, error)
	GetByHost(ctx context.Context, orgID, host string) (Connector, error)
	ListByOrg(ctx context.Context, orgID string) ([]Connector, error)
	Update(ctx context.Context, c Connector) error
	Delete(ctx context.Context, id string) error
}

var connectorsT = newTable("connectors", func(c *Connector) []*string { return []*string{&c.Config} })

type connectors struct{ crud[Connector] }

func (s connectors) GetByHost(ctx context.Context, orgID, host string) (Connector, error) {
	return s.one(ctx, "org_id = ? AND host = ?", orgID, host)
}

func (s connectors) ListByOrg(ctx context.Context, orgID string) ([]Connector, error) {
	return s.many(ctx, "org_id = ?", orgID)
}
