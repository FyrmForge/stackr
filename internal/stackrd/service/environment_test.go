package service

import (
	"context"
	"testing"

	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

type envStore struct {
	repo.Store
	stack   *repo.Stack
	bySlug  *repo.Environment
	envs    []repo.Environment
	tiles   []repo.Tile
	created *repo.Environment
}

func (e *envStore) GetStack(context.Context, string) (*repo.Stack, error) { return e.stack, nil }

func (e *envStore) GetEnvironmentBySlug(context.Context, string, string) (*repo.Environment, error) {
	return e.bySlug, nil
}

func (e *envStore) ListEnvironmentsByStack(context.Context, string) ([]repo.Environment, error) {
	return e.envs, nil
}

func (e *envStore) ListTilesByEnv(context.Context, string) ([]repo.Tile, error) { return e.tiles, nil }

func (e *envStore) CreateEnvironment(_ context.Context, env *repo.Environment) error {
	e.created = env
	return nil
}

func envSvc(st repo.Store) *EnvironmentService {
	return NewEnvironmentService(st, nil, nil, nil, NewGateService(st))
}

// One rule set, where there used to be four plus a creator with none. The
// home environment's slug is the one that bit: the API reserved it, the panel
// did not, so an environment named "Stack" hit a UNIQUE index as a raw 500.
func TestCreateRules(t *testing.T) {
	st := &envStore{stack: &repo.Stack{ID: "s1"}}
	svc := envSvc(st)
	for _, name := range []string{"", "!!", "settings", "list", repo.HomeSlug} {
		if _, err := svc.Create(context.Background(), st.stack,
			CreateEnv{Name: name}, Actor{Via: "web"}); err == nil {
			t.Errorf("%q was accepted as an environment name", name)
		}
	}
	if _, err := svc.Create(context.Background(), st.stack,
		CreateEnv{Name: "Staging"}, Actor{Via: "web"}); err != nil {
		t.Fatalf("a good name was refused: %v", err)
	}
	if st.created == nil || st.created.Slug != "staging" {
		t.Fatalf("created %+v", st.created)
	}
}

func TestCreateRefusesADuplicate(t *testing.T) {
	st := &envStore{stack: &repo.Stack{ID: "s1"}, bySlug: &repo.Environment{Name: "Staging"}}
	_, err := envSvc(st).Create(context.Background(), st.stack,
		CreateEnv{Name: "staging"}, Actor{Via: "web"})
	if _, ok := svcerr.IsConflict(err); !ok {
		t.Fatalf("want a conflict, got %v (%T)", err, err)
	}
}

// Creating an environment is structural, so the file owns it on a managed
// stack whatever ui_edits says.
func TestCreateRefusesOnAConfigManagedStack(t *testing.T) {
	st := &envStore{stack: &repo.Stack{ID: "s1", ConfigConnectorID: "c", ConfigRepo: "acme/infra",
		UIEditsMode: repo.UIEditsStage}}
	_, err := envSvc(st).Create(context.Background(), st.stack,
		CreateEnv{Name: "staging"}, Actor{Via: "web"})
	if _, ok := svcerr.IsConflict(err); !ok {
		t.Fatalf("want a conflict, got %v (%T)", err, err)
	}
	// ...but Adopt, which is the config engine and the PR hook, goes through.
	if _, err := envSvc(st).Adopt(context.Background(), st.stack, CreateEnv{Name: "pr 42"}); err != nil {
		t.Fatalf("Adopt was gated: %v", err)
	}
}

func TestDeleteRefusesTheLastEnvironment(t *testing.T) {
	st := &envStore{stack: &repo.Stack{ID: "s1"}, envs: []repo.Environment{{ID: "e1"}}}
	err := envSvc(st).Delete(context.Background(), st.stack, &repo.Environment{ID: "e1"},
		true, Actor{Via: "api"})
	if _, ok := svcerr.IsInvalid(err); !ok {
		t.Fatalf("want a refusal, got %v (%T)", err, err)
	}
}

// The running check the panel never had. force is the caller's answer to it.
func TestDeleteRefusesRunningTilesUnlessForced(t *testing.T) {
	st := &envStore{stack: &repo.Stack{ID: "s1"},
		envs:  []repo.Environment{{ID: "e1"}, {ID: "e2"}},
		tiles: []repo.Tile{{ID: "t1", Status: "running"}}}
	env := &repo.Environment{ID: "e1", Slug: "staging"}
	err := envSvc(st).Delete(context.Background(), st.stack, env, false, Actor{Via: "api"})
	if _, ok := svcerr.IsConflict(err); !ok {
		t.Fatalf("want a conflict, got %v (%T)", err, err)
	}
}

// Reset is the inverse gate: it exists for the managed stacks only.
func TestResetRequiresAConfigManagedStack(t *testing.T) {
	st := &envStore{stack: &repo.Stack{ID: "s1"}}
	err := envSvc(st).Reset(context.Background(), st.stack, &repo.Environment{ID: "e1"},
		true, Actor{Via: "api"})
	if _, ok := svcerr.IsInvalid(err); !ok {
		t.Fatalf("want a refusal, got %v (%T)", err, err)
	}
}

// SP1 on the two settings that used to be coerced.
func TestUpdateRefusesUnknownValues(t *testing.T) {
	svc := envSvc(&envStore{})
	bad := "mauve"
	if err := svc.Update(context.Background(), &repo.Environment{ID: "e1"},
		EnvPatch{Color: &bad}, Actor{Via: "web"}); err == nil {
		t.Error("an unknown colour was accepted")
	}
	pol := "whenever"
	if err := svc.Update(context.Background(), &repo.Environment{ID: "e1"},
		EnvPatch{ApplyPolicy: &pol}, Actor{Via: "web"}); err == nil {
		t.Error("an unknown apply policy was accepted")
	}
}
