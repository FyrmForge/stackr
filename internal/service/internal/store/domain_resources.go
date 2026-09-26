package store

import (
	"context"
	"time"
)

// DomainResource is a row of domain_resources: a host stackr names tiles
// under. Level says which owner id is set (instance: neither).
type DomainResource struct {
	ID                  string    `db:"id" json:"id"`
	Level               string    `db:"level" json:"level"` // instance | org | stack
	OrgID               *string   `db:"org_id" json:"org_id"`
	StackID             *string   `db:"stack_id" json:"stack_id"`
	Host                string    `db:"host" json:"host"`
	IncludeEnvOnDefault bool      `db:"include_env_on_default" json:"include_env_on_default"`
	ACMEEmail           string    `db:"acme_email" json:"acme_email"`
	Declared            bool      `db:"declared" json:"declared"`
	CreatedAt           time.Time `db:"created_at" json:"created_at"`
}

type DomainResourceStore interface {
	Create(ctx context.Context, r DomainResource) error
	Get(ctx context.Context, id string) (DomainResource, error)
	List(ctx context.Context) ([]DomainResource, error)
	Update(ctx context.Context, r DomainResource) error
	Delete(ctx context.Context, id string) error
}

var domainResourcesT = newTable[DomainResource]("domain_resources", nil)

type domainResources struct{ crud[DomainResource] }

func (s domainResources) List(ctx context.Context) ([]DomainResource, error) {
	return s.many(ctx, "1 = 1")
}
