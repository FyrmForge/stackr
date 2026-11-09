package opmember_test

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/opmember"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// R4: a replica never shares a node with the primary or another replica, the
// node must be named, and an instance has at most two standbys.
func TestCheck(t *testing.T) {
	prim := store.OperatedMember{Role: opmember.Primary}
	rep := func(node string) store.OperatedMember {
		return store.OperatedMember{Role: opmember.Replica, NodeID: node}
	}
	for _, c := range []struct {
		name string
		node string
		ms   []store.OperatedMember
		want string // "", "invalid" or "conflict"
	}{
		{"no node", "", []store.OperatedMember{prim}, "invalid"},
		{"primary's node", "a", []store.OperatedMember{prim}, "conflict"},
		{"other node", "b", []store.OperatedMember{prim}, ""},
		{"node of a replica", "b", []store.OperatedMember{prim, rep("b")}, "conflict"},
		{"second replica", "c", []store.OperatedMember{prim, rep("b")}, ""},
		{"third replica", "d", []store.OperatedMember{prim, rep("b"), rep("c")}, "conflict"},
	} {
		if got := kind(opmember.Check("a", c.node, c.ms)); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
}

func kind(err error) string {
	if _, ok := errs.IsInvalid(err); ok {
		return "invalid"
	}
	if _, ok := errs.IsConflict(err); ok {
		return "conflict"
	}
	if err != nil {
		return err.Error()
	}
	return ""
}

func TestMayStart(t *testing.T) {
	if err := opmember.MayStart(store.OperatedMember{}); err != nil {
		t.Errorf("unfenced: %v", err)
	}
	if err := opmember.MayStart(store.OperatedMember{Fenced: true}); kind(err) != "conflict" {
		t.Errorf("fenced: %v", err)
	}
}
