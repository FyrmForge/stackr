package store

import (
	"context"
	"time"
)

// Stack is a row of stacks.
type Stack struct {
	ID                string    `db:"id"`
	OrgID             string    `db:"org_id"`
	Name              string    `db:"name"`
	Slug              string    `db:"slug"`
	Description       string    `db:"description"`
	Settings          string    `db:"settings"`
	ConfigConnectorID string    `db:"config_connector_id"`
	ConfigRepo        string    `db:"config_repo"`
	ConfigBranch      string    `db:"config_branch"`
	ConfigPath        string    `db:"config_path"`
	Domains           string    `db:"domains"` // JSON, parsed by leaf/stack
	CreatedAt         time.Time `db:"created_at"`
}

type StackStore interface {
	Create(ctx context.Context, s Stack) error
	Get(ctx context.Context, id string) (Stack, error)
	GetBySlug(ctx context.Context, orgID, slug string) (Stack, error)
	ListByOrg(ctx context.Context, orgID string) ([]Stack, error)
	Update(ctx context.Context, s Stack) error
	Delete(ctx context.Context, id string) error
}

var stacksT = newTable[Stack]("stacks", nil)

type stacks struct{ crud[Stack] }

func (s stacks) GetBySlug(ctx context.Context, orgID, slug string) (Stack, error) {
	return s.one(ctx, "org_id = ? AND slug = ?", orgID, slug)
}

func (s stacks) ListByOrg(ctx context.Context, orgID string) ([]Stack, error) {
	return s.many(ctx, "org_id = ?", orgID)
}

// Environment is a row of environments.
type Environment struct {
	ID         string    `db:"id"`
	StackID    string    `db:"stack_id"`
	Name       string    `db:"name"`
	Slug       string    `db:"slug"`
	Type       string    `db:"type"`
	BaseEnvID  *string   `db:"base_env_id"`
	Settings   string    `db:"settings"`
	Color      string    `db:"color"`
	Position   int       `db:"position"`
	Network    string    `db:"network"`
	ReleaseID  *string   `db:"release_id"`
	FromKind   string    `db:"from_kind"`
	FromBranch string    `db:"from_branch"`
	Auto       bool      `db:"auto"`
	CreatedAt  time.Time `db:"created_at"`
}

type EnvironmentStore interface {
	Create(ctx context.Context, e Environment) error
	Get(ctx context.Context, id string) (Environment, error)
	GetBySlug(ctx context.Context, stackID, slug string) (Environment, error)
	ListByStack(ctx context.Context, stackID string) ([]Environment, error)
	Update(ctx context.Context, e Environment) error
	Delete(ctx context.Context, id string) error
}

var environmentsT = newTable[Environment]("environments", nil)

type environments struct{ crud[Environment] }

func (s environments) GetBySlug(ctx context.Context, stackID, slug string) (Environment, error) {
	return s.one(ctx, "stack_id = ? AND slug = ?", stackID, slug)
}

func (s environments) ListByStack(ctx context.Context, stackID string) ([]Environment, error) {
	return s.many(ctx, "stack_id = ?", stackID)
}
