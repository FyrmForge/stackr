package store

import (
	"context"
	"strings"
	"time"
)

// Stack is a row of stacks.
type Stack struct {
	ID                string    `db:"id" json:"id"`
	OrgID             string    `db:"org_id" json:"org_id"`
	Name              string    `db:"name" json:"name"`
	Slug              string    `db:"slug" json:"slug"`
	Description       string    `db:"description" json:"description"`
	Settings          string    `db:"settings" json:"settings"`
	ConfigConnectorID string    `db:"config_connector_id" json:"config_connector_id"`
	ConfigRepo        string    `db:"config_repo" json:"config_repo"`
	ConfigBranch      string    `db:"config_branch" json:"config_branch"`
	ConfigPath        string    `db:"config_path" json:"config_path"`
	CreatedAt         time.Time `db:"created_at" json:"created_at"`
	// NodeID pins new envs of this stack to a node; nil = no pin.
	NodeID *string `db:"node_id" json:"node_id"`
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
	ID         string    `db:"id" json:"id"`
	StackID    string    `db:"stack_id" json:"stack_id"`
	Name       string    `db:"name" json:"name"`
	Slug       string    `db:"slug" json:"slug"`
	Type       string    `db:"type" json:"type"`
	BaseEnvID  *string   `db:"base_env_id" json:"base_env_id"`
	Settings   string    `db:"settings" json:"settings"`
	Color      string    `db:"color" json:"color"`
	Position   int       `db:"position" json:"position"`
	Network    string    `db:"network" json:"network"`
	ReleaseID  *string   `db:"release_id" json:"release_id"`
	FromKind   string    `db:"from_kind" json:"from_kind"`
	FromBranch string    `db:"from_branch" json:"from_branch"`
	Auto       bool      `db:"auto" json:"auto"`
	CreatedAt  time.Time `db:"created_at" json:"created_at"`
	// Locked is the lock of an off-tier env; a tiered env uses its tier's.
	Locked bool `db:"locked" json:"locked"`
	// NodeID is the node the env runs on; it never changes under the env
	// (pins on the org and stack only decide where new envs go). Subnet is
	// the env's /24 from that node's range, "" until reserved.
	NodeID string `db:"node_id" json:"node_id"`
	Subnet string `db:"subnet" json:"subnet"`
}

type EnvironmentStore interface {
	Create(ctx context.Context, e Environment) error
	Get(ctx context.Context, id string) (Environment, error)
	GetBySlug(ctx context.Context, stackID, slug string) (Environment, error)
	ListByStack(ctx context.Context, stackID string) ([]Environment, error)
	ListByNode(ctx context.Context, nodeID string) ([]Environment, error)
	// SetSubnet writes the env's /24 and nothing else, so reserving one
	// cannot clobber a row the caller read earlier.
	SetSubnet(ctx context.Context, id, subnet string) error
	Update(ctx context.Context, e Environment) error
	Delete(ctx context.Context, id string) error
}

var environmentsT = newTable[Environment]("environments", nil)

type environments struct{ crud[Environment] }

// envUpdate is the env UPDATE without node_id and subnet: callers write back
// whole rows they read earlier, and a stale copy must not undo a reserved
// subnet. SetSubnet is the one writer of the subnet, and the node never
// changes under an env (moves, step 8, get their own setter).
var envUpdate = strings.NewReplacer(", node_id = :node_id", "", ", subnet = :subnet", "").Replace(environmentsT.update)

// Update writes every column of e except node_id and subnet, matched by id.
func (s environments) Update(ctx context.Context, e Environment) error {
	res, err := s.q.NamedExecContext(ctx, envUpdate, e)
	return affected(res, err)
}

func (s environments) GetBySlug(ctx context.Context, stackID, slug string) (Environment, error) {
	return s.one(ctx, "stack_id = ? AND slug = ?", stackID, slug)
}

func (s environments) ListByStack(ctx context.Context, stackID string) ([]Environment, error) {
	return s.many(ctx, "stack_id = ?", stackID)
}

func (s environments) SetSubnet(ctx context.Context, id, subnet string) error {
	res, err := s.q.ExecContext(ctx, `UPDATE environments SET subnet = ? WHERE id = ?`, subnet, id)
	return affected(res, err)
}

func (s environments) ListByNode(ctx context.Context, nodeID string) ([]Environment, error) {
	return s.many(ctx, "node_id = ?", nodeID)
}
