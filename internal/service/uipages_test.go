package service

import (
	"context"
	"testing"
)

// Only the envs on the controller are named, and only once a worker exists.
func TestControllerEnvs(t *testing.T) {
	ctx := context.Background()
	n := newTwoNodes(t)
	name, envs, err := n.orch.ControllerEnvs(ctx, n.stack)
	must(t, err)
	if name != n.ctl.Name || len(envs) != 1 || envs[0].ID != n.dev.ID {
		t.Fatalf("controller envs = %q %+v, want only dev on %q", name, envs, n.ctl.Name)
	}
	must(t, n.st.Nodes.Delete(ctx, "w1")) // back to one node: no warning
	if _, envs, _ = n.orch.ControllerEnvs(ctx, n.stack); len(envs) != 0 {
		t.Errorf("one node still warns: %+v", envs)
	}
}
