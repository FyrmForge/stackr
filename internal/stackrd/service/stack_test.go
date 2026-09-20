package service

import (
	"context"
	"errors"
	"testing"

	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

type stackStore struct {
	repo.Store
	bySlug   *repo.Stack
	stack    *repo.Stack
	created  *repo.Stack
	env      *repo.Environment
	updated  *repo.Stack
	torndown []string
	staged   []string
	conn     *repo.Connector
	envs     []repo.Environment
}

func (s *stackStore) GetStackBySlug(context.Context, string, string) (*repo.Stack, error) {
	return s.bySlug, nil
}
func (s *stackStore) GetStack(context.Context, string) (*repo.Stack, error) { return s.stack, nil }
func (s *stackStore) CreateStack(_ context.Context, st *repo.Stack) error {
	s.created = st
	return nil
}
func (s *stackStore) UpdateStack(_ context.Context, st *repo.Stack) error {
	s.updated = st
	return nil
}
func (s *stackStore) DeleteStack(_ context.Context, id string) error {
	s.torndown = append(s.torndown, "delete:"+id)
	return nil
}
func (s *stackStore) CreateEnvironment(_ context.Context, e *repo.Environment) error {
	s.env = e
	return nil
}
func (s *stackStore) ListEnvironmentsByStack(context.Context, string) ([]repo.Environment, error) {
	return s.envs, nil
}
func (s *stackStore) DeleteStagedByEnv(_ context.Context, envID string) error {
	s.staged = append(s.staged, envID)
	return nil
}
func (s *stackStore) GetConnector(context.Context, string) (*repo.Connector, error) {
	return s.conn, nil
}

func (s *stackStore) GetEnvironmentBySlug(context.Context, string, string) (*repo.Environment, error) {
	return nil, nil
}

type fakeStackOps struct{ down []string }

func (f *fakeStackOps) TeardownStack(_ context.Context, st *repo.Stack) error {
	f.down = append(f.down, st.ID)
	return nil
}

func stackSvcFor(st *stackStore, ops StackOps) *StackService {
	g := NewGateService(st)
	return NewStackService(st, ops, nil, NewEnvironmentService(st, nil, nil, nil, g), nil, g, nil)
}

// The duplicate slug is the row: neither surface pre-checked it, so a second
// stack with the same name hit UNIQUE (org_id, slug) and reached the user as a
// raw 500.
func TestCreateRefusesEmptyAndDuplicateSlugs(t *testing.T) {
	ctx := context.Background()
	st := &stackStore{}
	svc := stackSvcFor(st, nil)
	for _, name := range []string{"", "   ", "!!"} {
		if _, err := svc.Create(ctx, "org1", CreateStack{Name: name}); err == nil {
			t.Errorf("%q was accepted as a stack name", name)
		}
	}
	st.bySlug = &repo.Stack{ID: "other", Name: "Billing"}
	_, err := svc.Create(ctx, "org1", CreateStack{Name: "billing"})
	var conflict svcerr.Conflict
	if !errors.As(err, &conflict) {
		t.Fatalf("a duplicate slug should be a conflict, got %v", err)
	}

	st.bySlug = nil
	s, err := svc.Create(ctx, "org1", CreateStack{Name: "Billing API"})
	if err != nil {
		t.Fatalf("a good name was refused: %v", err)
	}
	if s.Slug != "billing-api" {
		t.Fatalf("slug %q", s.Slug)
	}
	// Every stack starts with a production environment, and it goes through
	// the environment service rather than being built by hand.
	if st.env == nil || st.env.Slug != "production" || st.env.StackID != s.ID {
		t.Fatalf("production env %+v", st.env)
	}
}

// Delete reloads the scheduler. The panel's own delete never did, so the cron
// and backup tables kept firing at tiles the cascade had removed. sched is nil
// here and its methods are nil-safe; what this asserts is the order — the
// teardown runs before the row goes.
func TestDeleteTearsDownBeforeDeleting(t *testing.T) {
	st := &stackStore{}
	ops := &fakeStackOps{}
	svc := stackSvcFor(st, ops)
	s := &repo.Stack{ID: "s1"}
	if err := svc.Delete(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if len(ops.down) != 1 || ops.down[0] != "s1" {
		t.Fatalf("teardown %v", ops.down)
	}
	if len(st.torndown) != 1 || st.torndown[0] != "delete:s1" {
		t.Fatalf("delete %v", st.torndown)
	}
}

// A binding takes a connector from the stack's own org only, normalises the
// repo, and drops every environment's staged rows. The config apply's own
// binding did none of the three.
func TestBindChecksConnectorOrgNormalisesRepoAndDropsStaged(t *testing.T) {
	ctx := context.Background()
	st := &stackStore{
		conn: &repo.Connector{ID: "c1", OrgID: "other-org"},
		envs: []repo.Environment{{ID: "e1"}, {ID: "e2"}},
	}
	svc := stackSvcFor(st, nil)
	s := &repo.Stack{ID: "s1", OrgID: "org1"}

	if err := svc.Bind(ctx, s, BindConfig{ConnectorID: "c1", Repo: "acme/infra"}); err == nil {
		t.Fatal("a connector from another org was accepted")
	}
	st.conn.OrgID = "org1"
	if err := svc.Bind(ctx, s, BindConfig{ConnectorID: "c1"}); err == nil {
		t.Fatal("an empty repo was accepted")
	}
	if err := svc.Bind(ctx, s, BindConfig{
		ConnectorID: "c1", Repo: "https://github.com/acme/infra.git", Branch: " main ",
	}); err != nil {
		t.Fatal(err)
	}
	if s.ConfigRepo != "acme/infra" {
		t.Fatalf("repo %q not normalised", s.ConfigRepo)
	}
	if s.ConfigBranch != "main" {
		t.Fatalf("branch %q", s.ConfigBranch)
	}
	if len(st.staged) != 2 {
		t.Fatalf("staged rows dropped for %v, wanted both envs", st.staged)
	}
}

// A person editing the binding of a stack an org file declared does not
// un-declare it: the file still declares it, and Unbind is what clears the
// flag.
func TestBindDoesNotClearOrgDeclared(t *testing.T) {
	st := &stackStore{conn: &repo.Connector{ID: "c1", OrgID: "org1"}}
	svc := stackSvcFor(st, nil)
	s := &repo.Stack{ID: "s1", OrgID: "org1", OrgDeclared: true}
	if err := svc.Bind(context.Background(), s, BindConfig{ConnectorID: "c1", Repo: "acme/infra"}); err != nil {
		t.Fatal(err)
	}
	if !s.OrgDeclared {
		t.Fatal("a panel rebind un-declared an org-declared stack")
	}
	if err := svc.Unbind(context.Background(), s); err != nil {
		t.Fatal(err)
	}
	if s.OrgDeclared {
		t.Fatal("unbind left the stack org-declared")
	}
}

// Reslug leaves the display name alone. The `moved:` path wrote Name and Slug
// both from the slug, so "Billing API" moved to `billing` came out displayed
// as "billing".
func TestReslugKeepsTheDisplayName(t *testing.T) {
	st := &stackStore{}
	svc := stackSvcFor(st, nil)
	s := &repo.Stack{ID: "s1", Name: "Billing API", Slug: "billing-api"}
	if err := svc.Reslug(context.Background(), s, "billing"); err != nil {
		t.Fatal(err)
	}
	if s.Slug != "billing" || s.Name != "Billing API" {
		t.Fatalf("reslug gave %q / %q", s.Name, s.Slug)
	}
}

// A config-managed stack refuses a rename: its file owns the name.
func TestUpdateRefusesARenameOnAManagedStack(t *testing.T) {
	st := &stackStore{stack: &repo.Stack{ID: "s1", ConfigRepo: "acme/infra", ConfigConnectorID: "c1"}}
	svc := stackSvcFor(st, nil)
	name := "New Name"
	err := svc.Update(context.Background(), st.stack, StackPatch{Name: &name}, Actor{Via: "api"})
	var conflict svcerr.Conflict
	if !errors.As(err, &conflict) {
		t.Fatalf("wanted a managed conflict, got %v", err)
	}
}
