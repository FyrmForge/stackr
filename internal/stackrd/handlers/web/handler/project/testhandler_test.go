package project

import (
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// testHandler is the page handler with every service it reads through wired
// to the test store and nothing else.
//
// The services' own dependencies stay nil: a read needs the store and the
// docker/proxy half is never reached. It is a function rather than a literal
// per test because point 19 adds a service to this list every slice, and eight
// copies of a growing struct literal is how a test ends up asserting against
// a nil field instead of a rule.
func testHandler(s repo.Store) *handler {
	gate := service.NewGateService(s)
	envs := service.NewEnvironmentService(s, nil, nil, nil, gate)
	return &handler{
		store:   s,
		envs:    envs,
		vars:    service.NewVariableService(s, nil, nil, nil),
		stacks:  service.NewStackService(s, nil, nil, envs, nil, gate, nil),
		tiles:   service.NewTileService(s, nil, nil, nil, nil, nil, gate),
		orgs:    service.NewOrgService(s),
		domains: service.NewDomainService(s, nil, gate),
		slices:  service.NewSliceService(s, nil, nil, nil),
		plans:   service.NewPlanService(s, nil, nil),
		deploys: service.NewDeployService(s, nil),
	}
}
