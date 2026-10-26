package canvas

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
)

// The order offers only moves the ladder takes: a promote env never moves
// to the top of the list, which builds from a branch.
func TestLadderOffersLegalMoves(t *testing.T) {
	es := []service.Environment{
		{ID: "a", Name: "dev", FromKind: "branch", FromBranch: "main"},
		{ID: "b", Name: "prod", FromKind: "promote"},
	}
	v := ladderView(es, "")
	if v.Rungs[0].Down != nil || v.Rungs[1].Up != nil {
		t.Errorf("a move that puts prod first is offered: %+v", v.Rungs)
	}
	es[1].FromKind = "branch"
	if v := ladderView(es, ""); v.Rungs[0].Down == nil || v.Rungs[1].Up == nil {
		t.Errorf("two branch envs cannot swap: %+v", v.Rungs)
	}
}
