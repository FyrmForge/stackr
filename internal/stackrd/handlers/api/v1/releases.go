package v1

// Releases and promotion.
//
// Promoting was a button on the canvas and nothing else, which meant a CI
// pipeline could build a commit and then had to ask a person to move it up the
// ladder. Same two operations here: what is promotable, and promote it.
//
// The rows are read from deployments rather than from the git log the panel
// renders. A script promoting a commit works from a sha it already has; the
// commit message and the distance-behind-head the panel shows come from GitHub
// or a local clone, and making the API depend on either would mean a promote
// that fails because a connector is rate-limited.

import (
	"net/http"
	"sort"
	"strings"

	"github.com/FyrmForge/stackr/internal/deploystate"

	"github.com/labstack/echo/v4"

	stackrmw "github.com/FyrmForge/stackr/internal/stackrd/handlers/middleware"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// releaseWindow is how many deployments per tile are read to build the list.
// The same depth the canvas uses: further back is history, not a release.
const releaseWindow = 5

// listReleases is the promotable set: every commit this stack has a deployment
// for, newest first, with what each rung runs and which plan is waiting.
func (a *API) listReleases(c echo.Context) error {
	s, err := a.stack(c, c.Param("id"))
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	envs, err := a.envs.ListForStack(ctx, s.ID)
	if err != nil {
		return err
	}
	type agg struct {
		built    bool
		building bool
		at       int64 // newest deployment time, for ordering
	}
	commits := map[string]*agg{}
	runs := map[string]string{} // env slug -> commit it runs
	for i := range envs {
		if envs[i].Type != "static" {
			continue
		}
		tiles, err := a.tiles.ListForEnv(ctx, envs[i].ID)
		if err != nil {
			return err
		}
		var newestDone *repo.Deployment
		for j := range tiles {
			if tiles[j].SourceType != "git" {
				continue
			}
			ds, err := a.deploys.ForTile(ctx, tiles[j].ID, releaseWindow)
			if err != nil {
				return err
			}
			for k := range ds {
				d := &ds[k]
				if d.CommitSHA == "" {
					continue
				}
				e := commits[d.CommitSHA]
				if e == nil {
					e = &agg{}
					commits[d.CommitSHA] = e
				}
				if t := d.CreatedAt.Unix(); t > e.at {
					e.at = t
				}
				switch d.Status {
				case "done":
					e.built = true
					if newestDone == nil || d.CreatedAt.After(newestDone.CreatedAt) {
						newestDone = d
					}
				default:
					if deploystate.IsLive(d.Status) {
						e.building = true
					}
				}
			}
		}
		if newestDone != nil {
			runs[envs[i].Slug] = newestDone.CommitSHA
		}
	}
	plans, _ := a.plans.ForStack(ctx, s.ID, 30)
	out := make([]releaseOut, 0, len(commits))
	for sha, e := range commits {
		row := releaseOut{Commit: sha, Built: e.built, Building: e.building,
			RunningOn: []string{}, at: e.at}
		for slug, c := range runs {
			if c == sha {
				row.RunningOn = append(row.RunningOn, slug)
			}
		}
		sort.Strings(row.RunningOn)
		for i := range plans {
			if plans[i].Status == "pending" && plans[i].CommitSHA == sha {
				row.PendingPlan = plans[i].ID
				break
			}
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].at > out[j].at })
	return c.JSON(http.StatusOK, out)
}

// promote moves a built commit onto a rung above the default environment.
//
// With a plan, both halves go into one job: the images must not move until the
// whole config has landed, and a restart between them would leave the promote
// orphaned. That is the same ApplyJob the panel's button enqueues.
//
// Force is the body's, defaulting to false. The panel passes true after its own
// confirmation dialogue; an API caller has not been shown one, so nothing here
// assumes it.
func (a *API) promote(c echo.Context) error {
	s, err := a.stack(c, c.Param("id"))
	if err != nil {
		return err
	}
	ctx := c.Request().Context()
	var in promoteIn
	if err := c.Bind(&in); err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, "invalid body")
	}
	env, err := a.releases.Target(ctx, s, c.Param("slug"))
	if err != nil {
		return stackrmw.HTTP(err)
	}
	job, err := a.releases.Promote(ctx, s, env, service.PromoteReq{
		Commit: in.Commit, PlanID: in.Plan, Force: in.Force,
	})
	if err != nil {
		return stackrmw.HTTP(err)
	}
	return c.JSON(http.StatusAccepted, promoteOut{Env: env.Slug, Commit: strings.TrimSpace(in.Commit), Plan: in.Plan, Job: job})
}
