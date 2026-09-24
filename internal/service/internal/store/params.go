package store

import (
	"context"
	"time"
)

// Param is a row of params. Value is plaintext here; the column is sealed.
type Param struct {
	ID         string    `db:"id"`
	ScopeKind  string    `db:"scope_kind"`
	ScopeID    string    `db:"scope_id"`
	Collection string    `db:"collection"`
	Name       string    `db:"name"`
	Kind       string    `db:"kind"`
	Value      string    `db:"value"`
	CreatedAt  time.Time `db:"created_at"`
	UpdatedAt  time.Time `db:"updated_at"`
}

type ParamStore interface {
	Create(ctx context.Context, p Param) error
	Get(ctx context.Context, id string) (Param, error)
	GetByName(ctx context.Context, scopeKind, scopeID, collection, name string) (Param, error)
	ListByScope(ctx context.Context, scopeKind, scopeID string) ([]Param, error)
	Update(ctx context.Context, p Param) error
	Delete(ctx context.Context, id string) error
}

var paramsT = newTable("params", func(p *Param) []*string { return []*string{&p.Value} })

type params struct{ crud[Param] }

func (s params) GetByName(ctx context.Context, scopeKind, scopeID, collection, name string) (Param, error) {
	return s.one(ctx, "scope_kind = ? AND scope_id = ? AND collection = ? AND name = ?",
		scopeKind, scopeID, collection, name)
}

func (s params) ListByScope(ctx context.Context, scopeKind, scopeID string) ([]Param, error) {
	return s.many(ctx, "scope_kind = ? AND scope_id = ?", scopeKind, scopeID)
}
