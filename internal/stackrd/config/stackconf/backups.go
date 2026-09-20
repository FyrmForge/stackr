package stackconf

// Backup schedules in the stack file.
//
// A schedule was panel-only, which meant a config-managed stack could describe
// every part of a database except the one that keeps its data. The file owns it
// now like any other key: declared means it exists, stopped being declared
// means it goes.
//
// The destination is a reference, never a literal: a destination carries the
// bucket credentials and the file is in git. Which destination a name resolves
// to is a store question, so it is checked here at plan time rather than at
// parse time.

import (
	"context"
	"fmt"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/backup"
	"github.com/FyrmForge/stackr/internal/stackrd/service"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// wantBackup is one tile's declared schedule, resolved against the store.
type wantBackup struct {
	Env  string
	Tile string
	Conf BackupConf
	Dest *repo.BackupDestination
	Kind string // derived: dump for a managed database, volume otherwise
}

// backupKind is derived, never declared: a managed database is dumped with its
// engine's own tool, anything else with a volume is a tar of that volume. The
// file repeating it would only ever be a second thing to disagree with.
func backupKind(tc TileConf) string {
	if tc.Type == "managed" {
		return repo.BackupDump
	}
	return repo.BackupVolume
}

// wantedBackups resolves every declared schedule in the walked environments,
// appending a plan error per reference that does not resolve.
func (pl Planner) wantedBackups(ctx context.Context, stack *repo.Stack, r *Resolved, onlyEnv string, skip map[string]bool, errs *[]string) []wantBackup {
	var out []wantBackup
	for _, envName := range r.EnvOrder {
		if (onlyEnv != "" && envName != onlyEnv) || skip[envName] {
			continue
		}
		re := r.Envs[envName]
		for _, tileName := range sortedTileNames(re.Tiles) {
			tc := re.Tiles[tileName]
			if tc.Backup == nil {
				continue
			}
			ref, err := backupDestRef(tc.Backup.Dest)
			if err != nil {
				*errs = append(*errs, fmt.Sprintf("env %s tile %s: backup: %v", envName, tileName, err))
				continue
			}
			d, err := backup.ResolveNamed(ctx, pl.Store, stack.OrgID, ref.Scope, ref.Name)
			if err != nil {
				*errs = append(*errs, fmt.Sprintf("env %s tile %s: backup: %v", envName, tileName, err))
				continue
			}
			out = append(out, wantBackup{Env: envName, Tile: tileName,
				Conf: *tc.Backup, Dest: d, Kind: backupKind(tc)})
		}
	}
	return out
}

// planBackups diffs declared schedules against stored ones. A tile that has a
// row and no longer declares one is a delete: the file owns the schedule, so
// dropping the block is how you turn a backup off for good. `enabled: false`
// is how you turn it off and keep it.
func (pl Planner) planBackups(ctx context.Context, stack *repo.Stack, r *Resolved, p *Plan, onlyEnv string, skip map[string]bool) {
	want := pl.wantedBackups(ctx, stack, r, onlyEnv, skip, &p.Errors)
	byTile := map[string]wantBackup{}
	for _, w := range want {
		byTile[w.Env+"/"+w.Tile] = w
	}
	for _, envName := range r.EnvOrder {
		if (onlyEnv != "" && envName != onlyEnv) || skip[envName] {
			continue
		}
		env, err := pl.Store.GetEnvironmentBySlug(ctx, stack.ID, envName)
		if err != nil || env == nil {
			// Not created yet: every declared schedule is a create, and the
			// apply writes them once the env exists.
			for _, tileName := range sortedTileNames(r.Envs[envName].Tiles) {
				if w, ok := byTile[envName+"/"+tileName]; ok {
					p.Changes = append(p.Changes, backupChange("create", w))
				}
			}
			continue
		}
		for _, tileName := range sortedTileNames(r.Envs[envName].Tiles) {
			t, err := pl.Store.GetTileBySlug(ctx, env.ID, tileName)
			if err != nil || t == nil {
				if w, ok := byTile[envName+"/"+tileName]; ok {
					p.Changes = append(p.Changes, backupChange("create", w))
				}
				continue
			}
			cur, err := pl.Store.ListBackupsByTile(ctx, t.ID)
			if err != nil {
				continue
			}
			w, declared := byTile[envName+"/"+tileName]
			// The whole list, not cur[0]. One `backup:` block is the tile's
			// full desired set, so a second schedule made in the panel is a
			// removal the plan has to show — it used to be invisible to the
			// plan and then deleted anyway when the block went, which is the
			// plan saying one thing and doing another.
			switch {
			case declared && len(cur) == 0:
				p.Changes = append(p.Changes, backupChange("create", w))
			case declared:
				old := backupSummary(&cur[0], destName(ctx, pl.Store, cur[0].DestinationID))
				if old != wantSummary(w) || len(cur) > 1 {
					ch := backupChange("update", w)
					ch.Old = old
					if len(cur) > 1 {
						ch.Note = fmt.Sprintf("the file owns this tile's schedules; %d extra one(s) made outside it are removed", len(cur)-1)
					}
					p.Changes = append(p.Changes, ch)
				}
			case len(cur) > 0:
				p.Changes = append(p.Changes, Change{Kind: "delete", Env: envName, Tile: tileName,
					Field: "backup", Old: backupSummary(&cur[0], destName(ctx, pl.Store, cur[0].DestinationID)),
					Note: "stops backing this tile up; archives already in the bucket are kept"})
			}
		}
	}
}

func backupChange(kind string, w wantBackup) Change {
	return Change{Kind: kind, Env: w.Env, Tile: w.Tile, Field: "backup",
		New:  wantSummary(w),
		Note: "the schedule is the file's now; removing the block stops the backups"}
}

// wantSummary and backupSummary render the same fields the same way, so a plan
// row only appears when something actually differs.
func wantSummary(w wantBackup) string {
	b := &repo.Backup{Kind: w.Kind, Cron: w.Conf.Schedule, Timezone: w.Conf.TZ,
		KeepLatest: w.Conf.Keep, ContainerMode: w.Conf.Mode, Enabled: w.Conf.On()}
	return backupSummary(b, w.Dest.Name)
}

func backupSummary(b *repo.Backup, dest string) string {
	s := b.Kind + " to " + dest + " on " + b.Schedule() + ", keep " + keepLabel(b.KeepLatest)
	if b.Kind == repo.BackupVolume && b.ContainerMode != "" {
		s += ", " + b.ContainerMode
	}
	if !b.Enabled {
		s += ", disabled"
	}
	return s
}

func keepLabel(n int) string {
	if n == 0 {
		return "all"
	}
	return fmt.Sprintf("%d", n)
}

// destName resolves a stored schedule's destination for display. A destination
// that has been deleted under the row reads as "(missing)" rather than an id
// nobody can look up.
func destName(ctx context.Context, store repo.Store, id string) string {
	d, err := store.GetBackupDestination(ctx, id)
	if err != nil || d == nil {
		return "(missing)"
	}
	return d.Name
}

// applyBackups writes the declared schedules and removes the ones the file has
// stopped declaring.
func (a Applier) applyBackups(ctx context.Context, stack *repo.Stack, r *Resolved, onlyEnv string, skip map[string]bool) error {
	store := a.Planner.Store
	var errs []string
	want := a.Planner.wantedBackups(ctx, stack, r, onlyEnv, skip, &errs)
	if len(errs) > 0 {
		// The plan already reported these and the gate refused them; reaching
		// here means a path that skipped the plan, so fail loudly.
		return fmt.Errorf("%s", errs[0])
	}
	byTile := map[string]wantBackup{}
	for _, w := range want {
		byTile[w.Env+"/"+w.Tile] = w
	}
	for _, envName := range r.EnvOrder {
		if (onlyEnv != "" && envName != onlyEnv) || skip[envName] {
			continue
		}
		env, err := store.GetEnvironmentBySlug(ctx, stack.ID, envName)
		if err != nil || env == nil {
			continue
		}
		for _, tileName := range sortedTileNames(r.Envs[envName].Tiles) {
			t, err := store.GetTileBySlug(ctx, env.ID, tileName)
			if err != nil || t == nil {
				continue
			}
			cur, err := store.ListBackupsByTile(ctx, t.ID)
			if err != nil {
				return err
			}
			w, declared := byTile[envName+"/"+tileName]
			if !declared {
				for i := range cur {
					if err := store.DeleteBackup(ctx, cur[i].ID); err != nil {
						return err
					}
				}
				continue
			}
			keep := w.Conf.Keep
			enabled := w.Conf.On()
			spec := service.ScheduleSpec{
				Dest: w.Dest.ID, Kind: w.Kind, Mode: w.Conf.Mode,
				Cron: &w.Conf.Schedule, Timezone: &w.Conf.TZ,
				Keep: &keep, Enabled: &enabled,
			}
			if len(cur) == 0 {
				// Adopt, not Create: the gate this write would be checked
				// against is the one protecting the file, and this is the
				// file. The rules are the same otherwise — including
				// backup.Validate and the volume guard, which this path
				// skipped entirely.
				if _, err := a.Schedules.Adopt(ctx, t, spec); err != nil {
					return fmt.Errorf("tile %s: backup: %w", tileName, err)
				}
				continue
			}
			b := cur[0]
			if err := a.Schedules.Reconcile(ctx, &b, t, spec); err != nil {
				return fmt.Errorf("tile %s: backup: %w", tileName, err)
			}
			// One `backup:` block is the tile's whole desired set, so anything
			// beyond the first row was made outside the file and goes.
			for i := 1; i < len(cur); i++ {
				if err := store.DeleteBackup(ctx, cur[i].ID); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
