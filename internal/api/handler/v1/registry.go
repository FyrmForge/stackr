package v1

import (
	"github.com/labstack/echo/v4"

	"github.com/FyrmForge/stackr/internal/service"
	"github.com/FyrmForge/stackr/internal/service/errs"
)

// RegistryRepos lists the org's image repositories.
func (h *H) RegistryRepos() Endpoint {
	return Get(func(c echo.Context) ([]service.RegistryRepo, error) {
		return list(h.Orch.OrgRepositories(rc(c), orgID(c)))
	})
}

// RegistryVersions lists one repo's versions (?repo=), newest first, with the
// envs that run each.
func (h *H) RegistryVersions() Endpoint {
	return Get(func(c echo.Context) ([]service.RegistryVersion, error) {
		return list(h.Orch.RegistryVersions(rc(c), orgID(c), c.QueryParam("repo")))
	}).Q("repo")
}

// DeleteRegistryVersion removes the version ?repo= and ?tag= name. One an env
// runs is a 409 naming the envs, unless ?force=true.
func (h *H) DeleteRegistryVersion() Endpoint {
	return Done(func(c echo.Context, _ None) error {
		repo, tag := c.QueryParam("repo"), c.QueryParam("tag")
		if repo == "" || tag == "" {
			return errs.Invalidf("repo", "repo and tag are required")
		}
		return h.Orch.DeleteRegistryVersion(rc(c), orgID(c), repo, tag, c.QueryParam("force") == "true")
	}).Q("repo", "tag", "force")
}

// RegistryOverview (server admin) is the registry's total, per org, and the
// last cleanup.
func (h *H) RegistryOverview() Endpoint {
	return Get(func(c echo.Context) (service.RegistryOverview, error) { return h.Orch.RegistryOverview(rc(c)) })
}

// CleanRegistry (server admin) queues a cleanup sweep.
func (h *H) CleanRegistry() Endpoint {
	return Job(func(c echo.Context, _ None) (service.Job, error) { return h.Orch.CleanRegistryNow(rc(c), who(c)) })
}

// ObserveStatus (server admin) is metrics history: on or off, its knobs, the
// container, its data size and the controller's free disk. The knobs are
// settings (observe_*).
func (h *H) ObserveStatus() Endpoint {
	return Get(func(c echo.Context) (service.ObserveStatus, error) { return h.Orch.ObserveStatus(rc(c)) })
}
