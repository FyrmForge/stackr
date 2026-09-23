package envops_test

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/stackrd/config/envops"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/runtime"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// ops is Ops wired the way cmd/stackrd wires it — with the services it writes
// its rows through.
//
// The tests used to build `envops.Ops{Store: s}` inline. That was fine while
// every write here went straight to the store; it stopped being fine the
// moment the writes moved behind EnvironmentService and VariableService,
// because a missing field is a nil pointer at the first write rather than a
// compile error. One helper, so a service added to Ops is wired for every
// test at once instead of in four literals that drift apart.
func ops(t *testing.T, s repo.Store) envops.Ops {
	t.Helper()
	o := envops.Ops{
		Store: s,
		Vars:  service.NewVariableService(s, nil, nil, nil, nil),
	}
	// The same knot main.go has: the environment service tears down through
	// Ops, and Ops deletes its rows through the environment service.
	o.Envs = service.NewEnvironmentService(s, &o, nil, nil, service.NewGateService(s))
	return o
}

// opsRT is ops with a runtime, for the teardown that reaches containers.
func opsRT(t *testing.T, s repo.Store, rt *runtime.Runtime) envops.Ops {
	t.Helper()
	o := ops(t, s)
	o.RT = rt
	return o
}
