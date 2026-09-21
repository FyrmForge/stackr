package stackconf

import (
	"github.com/FyrmForge/stackr/internal/stackrd/config/envops"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// wiredOps is envops.Ops with the services an apply writes its rows through.
//
// The tests used to build `Applier{Planner: Planner{Store: store}}` and leave
// Ops at its zero value, which was harmless while every write in apply.go went
// straight to the store. It stopped being harmless the moment those writes
// moved behind the services: a zero Ops has nil services, and a nil service is
// a panic at the first write rather than a compile error.
//
// One helper, so a service added to Ops is wired for every test at once.
// Fourteen literals were fourteen chances to wire it differently — which is
// the same argument this whole extraction rests on, one level down.
func wiredOps(store repo.Store) envops.Ops {
	gate := service.NewGateService(store)
	o := envops.Ops{
		Store: store,
		Tiles: service.NewTileService(store, nil, nil, nil, nil, nil, gate),
		Vars:  service.NewVariableService(store, nil, nil, nil),
	}
	// The knot main.go has: the environment service tears down through Ops,
	// and Ops writes its rows back through the environment service.
	o.Envs = service.NewEnvironmentService(store, &o, nil, nil, gate)
	return o
}

// applier is an Applier wired the way cmd/stackrd wires one. The infra below
// the services (cluster, proxy, scheduler, engine) stays nil: what these tests
// are about is the rule, not the container.
func applier(p Planner) Applier {
	return Applier{Planner: p, Ops: wiredOps(p.Store)}
}
