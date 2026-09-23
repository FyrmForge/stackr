package service

import (
	"context"
	"testing"

	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// varStore records what was written so a test can assert on the refusal
// having happened *before* any of it.
type varStore struct {
	repo.Store
	cur     []repo.Variable
	written []repo.Variable
	stack   *repo.Stack
}

func (v *varStore) ListVariables(context.Context, string, string) ([]repo.Variable, error) {
	return v.cur, nil
}

func (v *varStore) UpsertVariable(_ context.Context, r *repo.Variable) error {
	v.written = append(v.written, *r)
	return nil
}

func (v *varStore) AddAuditEvent(context.Context, *repo.AuditEvent) error { return nil }

func (v *varStore) GetStack(context.Context, string) (*repo.Stack, error) { return v.stack, nil }

// ClearWaiting walks the stack's tiles; nothing here is parked.
func (v *varStore) ListTilesByStack(context.Context, string) ([]repo.Tile, error) {
	return nil, nil
}

func varSvc(st repo.Store) *VariableService { return NewVariableService(st, nil, nil, nil, nil) }

// SP1: the panel's name rule is the rule. The API accepted any non-empty
// string, so a name with a space in it stored fine and then produced a
// container env line no shell could read.
func TestSetRefusesABadName(t *testing.T) {
	for _, name := range []string{"", "db url", "PG$", ".leading", "-leading"} {
		st := &varStore{}
		err := varSvc(st).Set(context.Background(), StackVars("s1"),
			[]VarWrite{{Name: name, Value: "x"}}, Actor{Via: "api"})
		if _, ok := svcerr.IsInvalid(err); !ok {
			t.Errorf("%q: want an invalid name, got %v", name, err)
		}
		if len(st.written) != 0 {
			t.Errorf("%q: refused but still wrote %d rows", name, len(st.written))
		}
	}
}

// A batch that is going to be refused must not half-apply: the bad name is
// second, and the good one before it must not reach the store.
func TestSetValidatesTheWholeBatchFirst(t *testing.T) {
	st := &varStore{}
	err := varSvc(st).Set(context.Background(), StackVars("s1"), []VarWrite{
		{Name: "GOOD", Value: "1"},
		{Name: "bad name", Value: "2"},
	}, Actor{Via: "api"})
	if _, ok := svcerr.IsInvalid(err); !ok {
		t.Fatalf("want an invalid name, got %v", err)
	}
	if len(st.written) != 0 {
		t.Fatalf("a refused batch wrote %d rows", len(st.written))
	}
}

// The mask is not a value. Only the API used to catch this, so a panel form
// that round-tripped a listing could replace a secret with three dots.
func TestSetRefusesTheMaskAsAValue(t *testing.T) {
	st := &varStore{}
	err := varSvc(st).Set(context.Background(), StackVars("s1"),
		[]VarWrite{{Name: "TOKEN", Value: Masked, Secret: true}}, Actor{Via: "api"})
	if _, ok := svcerr.IsInvalid(err); !ok {
		t.Fatalf("want a refusal, got %v", err)
	}
	if len(st.written) != 0 {
		t.Fatal("stored the mask over the secret")
	}
}

// Decided pick: generate mints only when there is nothing there. The panel
// used to overwrite, which is a silent credential rotation.
func TestGenerateLeavesALiveValueAlone(t *testing.T) {
	st := &varStore{cur: []repo.Variable{{Name: "TOKEN", Value: "already-set", Secret: true}},
		stack: &repo.Stack{ID: "s1"}}
	if err := varSvc(st).Set(context.Background(), StackVars("s1"),
		[]VarWrite{{Name: "TOKEN", Generate: true, Secret: true}}, Actor{Via: "web"}); err != nil {
		t.Fatal(err)
	}
	if len(st.written) != 0 {
		t.Fatalf("rotated a live secret: wrote %+v", st.written)
	}

	// ...and does mint when the name is empty.
	st = &varStore{cur: []repo.Variable{{Name: "TOKEN", Value: ""}}, stack: &repo.Stack{ID: "s1"}}
	if err := varSvc(st).Set(context.Background(), StackVars("s1"),
		[]VarWrite{{Name: "TOKEN", Generate: true, Secret: true}}, Actor{Via: "web"}); err != nil {
		t.Fatal(err)
	}
	if len(st.written) != 1 || st.written[0].Value == "" {
		t.Fatalf("did not mint a value for an empty secret: %+v", st.written)
	}
}

// The audit trail's two established spellings. A query for "what did this key
// do" matches on the api: prefix.
func TestActorAudit(t *testing.T) {
	for _, tc := range []struct {
		actor Actor
		want  string
	}{
		{Actor{Via: "api", Name: "ci"}, "api:ci"},
		{Actor{Via: "api"}, "api:?"},
		{Actor{Via: "web", Email: "a@b.c", Name: "A"}, "a@b.c"},
		{Actor{Via: "web", Name: "A"}, "A"},
		{Actor{}, "?"},
	} {
		if got := tc.actor.Audit(); got != tc.want {
			t.Errorf("%+v.Audit() = %q, want %q", tc.actor, got, tc.want)
		}
	}
}
