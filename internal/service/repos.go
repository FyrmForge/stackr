package service

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// The clones under <data>/repos: tileCheckout per git tile (shared by the
// envs whose tile has the same slug and repo), config-<stack>,
// orgconfig-<org> and serverconfig. Each goes when its owner does
// (forgetTileClone, DeleteStack, DeleteOrg); sweepRepos takes what those
// miss at boot (a renamed slug, a changed repo URL, a crash).

func (o *Orchestrator) reposDir() string { return filepath.Join(o.cfg.DataDir, "repos") }

// dropClone removes the clone name, one folder directly in <data>/repos,
// under its repo lock so a build or read using it finishes first. A
// symlink there goes itself; nothing behind it is touched.
func (o *Orchestrator) dropClone(name string) {
	if name == "" || name == "." || name == ".." || strings.ContainsRune(name, filepath.Separator) {
		return
	}
	dir := filepath.Join(o.reposDir(), name)
	unlock := o.repoLock(dir)
	defer unlock()
	fi, err := os.Lstat(dir)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return
	case err != nil:
	case fi.Mode()&os.ModeSymlink != 0:
		err = os.Remove(dir)
	default:
		err = os.RemoveAll(dir)
	}
	if err != nil {
		slog.Warn("repos: clone not removed", "clone", name, "err", err)
	}
}

// forgetTileClone is promote's Forget: t's row is gone, so its build clone
// goes too unless another tile (the same slug and repo in another env)
// still builds from it.
func (o *Orchestrator) forgetTileClone(ctx context.Context, t store.Tile) {
	if t.GitURL == "" {
		return
	}
	st, err := o.stacks.Get(ctx, t.StackID)
	if err != nil {
		return
	}
	name := tileCheckout(st, t)
	ts, err := o.tiles.ListByStack(ctx, st.ID)
	if err != nil {
		return
	}
	for _, other := range ts {
		if other.GitURL != "" && tileCheckout(st, other) == name {
			return
		}
	}
	o.dropClone(name)
}

// dropStackClones removes a deleted stack's config clone and any tile
// clone of it still there.
func (o *Orchestrator) dropStackClones(stackID string) {
	o.dropClone("config-" + stackID)
	es, _ := os.ReadDir(o.reposDir())
	for _, e := range es {
		if strings.HasPrefix(e.Name(), "tile-"+stackID+"-") {
			o.dropClone(e.Name())
		}
	}
}

// sweepRepos removes every clone no org, stack, tile or the server config
// maps to now. It runs at boot before the job runner starts, so nothing
// uses one; any failed read leaves every clone where it is.
func (o *Orchestrator) sweepRepos(ctx context.Context) error {
	es, err := os.ReadDir(o.reposDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	keep := map[string]bool{}
	b, err := o.ServerConfigBinding(ctx)
	if err != nil {
		return err
	}
	keep["serverconfig"] = b.Repo != ""
	ogs, err := o.orgs.ListAll(ctx)
	if err != nil {
		return err
	}
	for _, og := range ogs {
		keep["orgconfig-"+og.ID] = og.ConfigRepo != ""
		sts, err := o.stacks.List(ctx, og.ID)
		if err != nil {
			return err
		}
		for _, st := range sts {
			keep["config-"+st.ID] = st.ConfigRepo != ""
			ts, err := o.tiles.ListByStack(ctx, st.ID)
			if err != nil {
				return err
			}
			for _, t := range ts {
				if t.GitURL != "" {
					keep[tileCheckout(st, t)] = true
				}
			}
		}
	}
	for _, e := range es {
		if !keep[e.Name()] {
			o.dropClone(e.Name())
		}
	}
	return nil
}
