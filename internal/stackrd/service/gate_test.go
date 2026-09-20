package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// gateStore answers GetStack and nothing else. The embedded nil interface
// panics on any other call, which is the point: if Gate grows a second store
// call, this test says so instead of quietly passing.
type gateStore struct {
	repo.Store
	s   *repo.Stack
	err error
}

func (g gateStore) GetStack(context.Context, string) (*repo.Stack, error) { return g.s, g.err }

func managed(mode string) *repo.Stack {
	return &repo.Stack{ID: "s1", ConfigConnectorID: "c1", ConfigRepo: "acme/infra", UIEditsMode: mode}
}

func TestGate(t *testing.T) {
	ui := &repo.Stack{ID: "s1"}
	cases := []struct {
		name      string
		store     gateStore
		kind      GateKind
		via       Surface
		wantStage bool
		wantErr   error
		conflict  bool
	}{
		// On a stack nobody's config file owns, staging is purely the
		// surface's habit: the canvas queues for review, a script does not.
		{"canvas stages a field edit", gateStore{s: ui}, GateFieldEdit, SurfaceCanvas, true, nil, false},
		{"canvas stages a delete too", gateStore{s: ui}, GateStructural, SurfaceCanvas, true, nil, false},
		{"api writes a field edit straight through", gateStore{s: ui}, GateFieldEdit, SurfaceDirect, false, nil, false},
		{"api writes a delete straight through", gateStore{s: ui}, GateStructural, SurfaceDirect, false, nil, false},

		{"config-managed blocks field edits", gateStore{s: managed(repo.UIEditsBlock)}, GateFieldEdit, SurfaceCanvas, false, nil, true},
		{"config-managed stages field edits when asked", gateStore{s: managed(repo.UIEditsStage)}, GateFieldEdit, SurfaceCanvas, true, nil, false},
		// The row this unification exists to close: the panel staged and the
		// API 409'd on the identical edit to the same stack.
		{"stage mode forces the api to stage as well", gateStore{s: managed(repo.UIEditsStage)}, GateFieldEdit, SurfaceDirect, true, nil, false},
		// The row the GateKind split exists for: a stack that stages field
		// edits must still refuse a create or a delete, from either surface.
		{"stage mode still refuses structural", gateStore{s: managed(repo.UIEditsStage)}, GateStructural, SurfaceCanvas, false, nil, true},
		{"stage mode still refuses structural over the api", gateStore{s: managed(repo.UIEditsStage)}, GateStructural, SurfaceDirect, false, nil, true},

		{"missing stack is not found", gateStore{s: nil}, GateFieldEdit, SurfaceCanvas, false, svcerr.ErrNotFound, false},
		{"lookup error fails closed", gateStore{err: errors.New("db down")}, GateStructural, SurfaceDirect, false, svcerr.ErrNotFound, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			stage, err := NewGateService(c.store).Gate(context.Background(), "s1", c.kind, c.via)
			if stage != c.wantStage {
				t.Errorf("stage = %v, want %v", stage, c.wantStage)
			}
			switch {
			case c.conflict:
				cf, ok := svcerr.IsConflict(err)
				if !ok {
					t.Fatalf("want a conflict, got %v (%T)", err, err)
				}
				if cf.Msg == "" || !strings.Contains(cf.Msg, "acme/infra") {
					t.Errorf("the refusal must name the file that owns the stack, got %q", cf.Msg)
				}
			case c.wantErr != nil:
				if !errors.Is(err, c.wantErr) {
					t.Errorf("err = %v, want %v", err, c.wantErr)
				}
			default:
				if err != nil {
					t.Errorf("unexpected err %v", err)
				}
			}
		})
	}
}

// ManagedConflict with no repo on the stack still has to say something a
// person can act on.
func TestManagedConflictWithoutRepo(t *testing.T) {
	err := ManagedConflict(&repo.Stack{ID: "s1", ConfigConnectorID: "c1"})
	cf, ok := svcerr.IsConflict(err)
	if !ok || !strings.Contains(cf.Msg, "the config file") {
		t.Errorf("got %q", cf.Msg)
	}
}
