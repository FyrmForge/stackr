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
	// PlacementID pins the stack's envs to a placement; nil = no pin.
	PlacementID *string `db:"placement_id" json:"placement_id"`
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
	// NodeID is the node the env runs on; only a move changes it. Subnet is
	// the env's /24 from the cluster pool, "" until reserved.
	NodeID string `db:"node_id" json:"node_id"`
	Subnet string `db:"subnet" json:"subnet"`
	// PlacementID is the placement the env belongs to; NodeID is where it
	// actually runs (they differ mid move, or "" while the placement has no
	// node).
	PlacementID string `db:"placement_id" json:"placement_id"`
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
	// SetNode is the one writer of the node, for moves. The subnet stays:
	// it comes from the cluster pool, so the env keeps its network (and its
	// tiles their VIPs) on the new node.
	SetNode(ctx context.Context, id, nodeID string) error
	// SetPlacement is the one writer of the placement.
	SetPlacement(ctx context.Context, id, placementID string) error
	ListByPlacement(ctx context.Context, placementID string) ([]Environment, error)
	// Subnets is every env's network, all nodes: the blocks of the cluster
	// pool envs hold.
	Subnets(ctx context.Context) ([]string, error)
	// AdoptUnplaced gives every env with no node this one (envs made before
	// nodes existed) and reports how many.
	AdoptUnplaced(ctx context.Context, nodeID string) (int64, error)
	Update(ctx context.Context, e Environment) error
	Delete(ctx context.Context, id string) error
}

var environmentsT = newTable[Environment]("environments", nil)

type environments struct{ crud[Environment] }

// envUpdate is the env UPDATE without node_id, subnet and placement_id:
// callers write back whole rows they read earlier, and a stale copy must not
// undo a reserved subnet or a move. SetSubnet, SetNode and SetPlacement are
// their one writers.
var envUpdate = strings.NewReplacer(", node_id = :node_id", "", ", subnet = :subnet", "", ", placement_id = :placement_id", "").Replace(environmentsT.update)

// Update writes every column of e except node_id, subnet and placement_id,
// matched by id.
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

func (s environments) SetNode(ctx context.Context, id, nodeID string) error {
	res, err := s.q.ExecContext(ctx, `UPDATE environments SET node_id = ? WHERE id = ?`, nodeID, id)
	return affected(res, err)
}

func (s environments) SetPlacement(ctx context.Context, id, placementID string) error {
	res, err := s.q.ExecContext(ctx, `UPDATE environments SET placement_id = ? WHERE id = ?`, placementID, id)
	return affected(res, err)
}

func (s environments) ListByPlacement(ctx context.Context, placementID string) ([]Environment, error) {
	return s.many(ctx, "placement_id = ?", placementID)
}

func (s environments) Subnets(ctx context.Context) ([]string, error) {
	var out []string
	err := s.q.SelectContext(ctx, &out, `SELECT subnet FROM environments WHERE subnet != ''`)
	return out, mapErr(err)
}

func (s environments) AdoptUnplaced(ctx context.Context, nodeID string) (int64, error) {
	res, err := s.q.ExecContext(ctx, `UPDATE environments SET node_id = ? WHERE node_id = ''`, nodeID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func (s environments) ListByNode(ctx context.Context, nodeID string) ([]Environment, error) {
	return s.many(ctx, "node_id = ?", nodeID)
}
