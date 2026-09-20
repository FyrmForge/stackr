package service

import (
	"context"
	"testing"

	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// instStore answers the lookups a refusal needs and nothing else: every case
// here must refuse before any write, so a call past that point panics on the
// nil embedded interface, which is the assertion.
type instStore struct {
	repo.Store
	stack *repo.Stack
	held  []repo.Provision
	bySlg *repo.Tile
}

func (s instStore) GetStack(context.Context, string) (*repo.Stack, error) { return s.stack, nil }

func (s instStore) GetTileBySlug(context.Context, string, string) (*repo.Tile, error) {
	return s.bySlg, nil
}

func (s instStore) ListProvisionsByInstance(context.Context, string) ([]repo.Provision, error) {
	return s.held, nil
}

func instSvc(st repo.Store) *ManagedInstanceService {
	return NewManagedInstanceService(st, nil, nil, NewGateService(st), nil)
}

func pgTile() *repo.Tile {
	return &repo.Tile{ID: "i1", StackID: "s1", EnvironmentID: "e1", Slug: "pg",
		Engine: "postgres", Kind: "service", Status: "running"}
}

// SP1: a scope name nobody recognises is refused, not folded to "env". The
// panel's fall-through turned a typo into a silent narrowing of an instance
// other stacks were already provisioning from.
func TestCreateRefusesAnUnknownScope(t *testing.T) {
	st := instStore{stack: &repo.Stack{ID: "s1", OrgID: "o1"}}
	_, err := instSvc(st).Create(context.Background(),
		&repo.Tile{StackID: "s1", EnvironmentID: "e1", Name: "pg", Engine: "postgres"},
		"team", nil, Actor{Via: "api"})
	inv, ok := svcerr.IsInvalid(err)
	if !ok || inv.Field != "scope" {
		t.Fatalf("want an invalid scope, got %v (%T)", err, err)
	}
}

// The API accepted a name with no slug in it where the panel refused one, so
// a database called "!!" landed with no address anything could reference.
func TestCreateRefusesANameWithNoSlug(t *testing.T) {
	st := instStore{stack: &repo.Stack{ID: "s1", OrgID: "o1"}}
	_, err := instSvc(st).Create(context.Background(),
		&repo.Tile{StackID: "s1", EnvironmentID: "e1", Name: "!!", Engine: "postgres"},
		"", nil, Actor{Via: "api"})
	if inv, ok := svcerr.IsInvalid(err); !ok || inv.Field != "name" {
		t.Fatalf("want an invalid name, got %v (%T)", err, err)
	}
}

func TestCreateRefusesADuplicateSlug(t *testing.T) {
	st := instStore{stack: &repo.Stack{ID: "s1", OrgID: "o1"}, bySlg: &repo.Tile{Name: "PG"}}
	_, err := instSvc(st).Create(context.Background(),
		&repo.Tile{StackID: "s1", EnvironmentID: "e1", Name: "pg", Engine: "postgres"},
		"", nil, Actor{Via: "api"})
	if _, ok := svcerr.IsConflict(err); !ok {
		t.Fatalf("want a conflict, got %v (%T)", err, err)
	}
}

// The gate drift row: the panel let an org-scoped instance be edited on a
// config-managed stack (the file cannot declare one), the API refused it.
// Settings take the exception; the scope itself never does.
func TestOrgScopeIsExemptFromTheFileGateButItsScopeIsNot(t *testing.T) {
	st := instStore{stack: &repo.Stack{ID: "s1", ConfigConnectorID: "c", ConfigRepo: "acme/infra"}}
	svc := instSvc(st)
	org := pgTile()
	org.ScopeKind = "org"

	if err := svc.fileUnowned(context.Background(), org); err != nil {
		t.Fatalf("an org-scoped instance is not the file's: %v", err)
	}
	if err := svc.SetScope(context.Background(), org, "env", Actor{Via: "api"}); err == nil {
		t.Fatal("a scope change on a managed stack must refuse")
	}
	env := pgTile()
	if err := svc.fileUnowned(context.Background(), env); err == nil {
		t.Fatal("an env-scoped instance on a managed stack must refuse")
	}
}

func TestValidate(t *testing.T) {
	svc := instSvc(instStore{})
	for _, tc := range []struct {
		name  string
		edit  func(*repo.Tile)
		field string
	}{
		{"port", func(t *repo.Tile) { t.ExternalPort = 70000 }, "external_port"},
		{"negative port", func(t *repo.Tile) { t.ExternalPort = -1 }, "external_port"},
		{"cpu", func(t *repo.Tile) { t.CPULimit = -1 }, "cpu_limit"},
		{"mem", func(t *repo.Tile) { t.MemLimitMB = -1 }, "mem_limit_mb"},
		{"shm", func(t *repo.Tile) { t.ShmSizeMB = -1 }, "shm_size_mb"},
		{"policy", func(t *repo.Tile) { t.UpdatePolicy = "sometimes" }, "update_policy"},
		{"engine", func(t *repo.Tile) { t.Engine = "cassandra" }, "engine"},
	} {
		d := pgTile()
		tc.edit(d)
		inv, ok := svcerr.IsInvalid(svc.Validate(d))
		if !ok || inv.Field != tc.field {
			t.Errorf("%s: want an invalid %s, got %v", tc.name, tc.field, svc.Validate(d))
		}
	}

	// The two normalisations that are not the user's choice.
	d := pgTile()
	d.MemLimitMB = 3
	if err := svc.Validate(d); err != nil || d.MemLimitMB != 6 {
		t.Errorf("a memory cap under docker's floor should be raised to it, got %d (%v)", d.MemLimitMB, err)
	}
	d = pgTile()
	if err := svc.Validate(d); err != nil || d.ImageRef == "" || d.UpdatePolicy != "off" {
		t.Errorf("empty image and policy should take their defaults, got %q/%q (%v)", d.ImageRef, d.UpdatePolicy, err)
	}
}

// The held-slices refusal: deleting an instance destroys every consumer's
// data with it, so the default is to refuse. The CLI used to force on every
// path, which made this unreachable.
func TestTearDownRefusesHeldSlicesUnlessForced(t *testing.T) {
	st := instStore{stack: &repo.Stack{ID: "s1"},
		held: []repo.Provision{{ID: "p1", DBName: "app"}}}
	err := instSvc(st).Delete(context.Background(), pgTile(), false, Actor{Via: "api"})
	if _, ok := svcerr.IsConflict(err); !ok {
		t.Fatalf("want a conflict, got %v (%T)", err, err)
	}
}

// A managed instance is only ever recreated by a change the container
// embodies: the panel redeployed on every save, whatever moved.
func TestAfterWriteOnlyRedeploysWhatTheContainerHolds(t *testing.T) {
	svc := instSvc(instStore{})
	// No dbs wired, so a redeploy would surface as ErrUnavailable.
	if err := svc.AfterWrite(context.Background(), pgTile(), Changed{"scope": true}); err != nil {
		t.Fatalf("a scope change must not recreate the container: %v", err)
	}
	if err := svc.AfterWrite(context.Background(), pgTile(), Changed{"external_port": true}); err == nil {
		t.Fatal("a port change must recreate the container")
	}
	stopped := pgTile()
	stopped.Status = "stopped"
	if err := svc.AfterWrite(context.Background(), stopped, Changed{"external_port": true}); err != nil {
		t.Fatalf("a deliberately stopped instance is left alone: %v", err)
	}
}
