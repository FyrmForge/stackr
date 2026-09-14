// Package cigate releases push deploys parked behind wait_for_ci: a janitor
// task polls GitHub's check runs + commit statuses for each waiting_ci
// deployment and turns green into a queued build, red into an error.
package cigate

import (
	"context"
	"errors"
	"time"

	"github.com/FyrmForge/stackr/internal/stackrd/handlers/notify"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/deploy"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/githubapp"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

const (
	// graceNoCI: how long an empty verdict (no runs, no statuses) can mean
	// "CI hasn't started yet" before it means "this repo has no CI" and the
	// deploy proceeds. Covers the post-push window before Actions creates
	// its check runs.
	graceNoCI = 3 * time.Minute
	// timeout: a commit whose checks never settle fails the deploy rather
	// than parking it forever.
	timeout = 30 * time.Minute
)

// Gate is the janitor task. One tick scans every parked row; verdicts are
// deduped per commit within a tick (several tiles often track one repo).
type Gate struct {
	Store    repo.Store
	Engine   *deploy.Engine
	GH       *githubapp.Client
	Notifier *notify.Notifier
}

func (g *Gate) Name() string { return "ci-gate" }

func (g *Gate) Run(ctx context.Context) (int64, error) {
	ds, err := g.Store.ListDeploymentsByStatus(ctx, "waiting_ci")
	if err != nil || len(ds) == 0 {
		return 0, err
	}
	type cached struct {
		verdict githubapp.CIVerdict
		detail  string
	}
	verdicts := map[string]cached{}
	var acted int64
	for i := range ds {
		d := &ds[i]
		t, err := g.Store.GetTile(ctx, d.TileID)
		if err != nil || t == nil {
			g.fail(ctx, d, nil, "tile is gone")
			acted++
			continue
		}
		if time.Since(d.CreatedAt) > timeout {
			g.fail(ctx, d, t, "CI didn't finish within 30 minutes")
			acted++
			continue
		}
		repoFull := githubapp.RepoFull(t.GitURL)
		if repoFull == "" || t.ConnectorID == "" {
			g.fail(ctx, d, t, "wait_for_ci needs a GitHub connector on the tile")
			acted++
			continue
		}
		cn, err := g.Store.GetConnector(ctx, t.ConnectorID)
		if err != nil || cn == nil {
			g.fail(ctx, d, t, "the tile's connector is gone")
			acted++
			continue
		}
		key := cn.ID + " " + repoFull + "@" + d.CommitSHA
		v, ok := verdicts[key]
		if !ok {
			verdict, detail, cerr := g.GH.CIState(ctx, cn, repoFull, d.CommitSHA)
			if errors.Is(cerr, githubapp.ErrNoChecksPerm) {
				g.fail(ctx, d, t, cerr.Error())
				acted++
				continue
			}
			if cerr != nil {
				continue // transient (network, rate limit), retry next tick
			}
			v = cached{verdict, detail}
			verdicts[key] = v
		}
		switch v.verdict {
		case githubapp.CIPassing:
			if err := g.Engine.Release(ctx, d); err == nil {
				acted++
			}
		case githubapp.CIFailing:
			g.fail(ctx, d, t, "CI failed: "+v.detail)
			acted++
		case githubapp.CINone:
			if time.Since(d.CreatedAt) > graceNoCI {
				// No CI on the repo at all: waiting longer buys nothing.
				if err := g.Engine.Release(ctx, d); err == nil {
					acted++
				}
			}
		}
		// pending: leave the row parked for the next tick
	}
	return acted, nil
}

func (g *Gate) fail(ctx context.Context, d *repo.Deployment, t *repo.Tile, msg string) {
	g.Engine.FailWaiting(ctx, d, msg)
	name := d.TileID
	if t != nil {
		name = t.Name
	}
	g.Notifier.Push(ctx, notify.KindDeployFailed, "Deploy blocked: "+name, msg, "/deployments/"+d.ID)
}
