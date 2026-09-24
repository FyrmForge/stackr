// Package schedule is the cron runner: backup schedules, the daily orphan
// retention job and the image-watch tick, on one robfig/cron. A tick
// enqueues through the func it was given and returns; it never runs the
// work. A missed tick is missed: nothing catches up after a restart.
package schedule

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/robfig/cron/v3"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Drivers are what a tick calls; each enqueues a job and returns.
type Drivers struct {
	// Schedules lists every backup schedule (the rows).
	Schedules func(ctx context.Context) ([]store.BackupSchedule, error)
	Backup    func(ctx context.Context, s store.BackupSchedule) error
	Orphans   func(ctx context.Context) error
	// Watch runs every minute; the image-watch flow's Due holds it to the
	// image_check_interval setting (DECIDE 34).
	Watch func(ctx context.Context) error
}

type Entry struct {
	Name, Spec string
	Fire       func(ctx context.Context) error
}

// Entries maps the rows to cron entries: the pure half, tested alone.
func Entries(d Drivers, scheds []store.BackupSchedule) []Entry {
	out := []Entry{
		{Name: "orphans", Spec: "@daily", Fire: d.Orphans},
		{Name: "image-watch", Spec: "@every 1m", Fire: d.Watch},
	}
	for _, s := range scheds {
		spec := s.Cron
		if s.Timezone != "" {
			spec = "CRON_TZ=" + s.Timezone + " " + s.Cron
		}
		out = append(out, Entry{Name: "backup " + s.ID, Spec: spec,
			Fire: func(ctx context.Context) error { return d.Backup(ctx, s) }})
	}
	return out
}

type Runner struct {
	d       Drivers
	mu      sync.Mutex
	cron    *cron.Cron
	entries []cron.EntryID
}

// New starts the cron once for the process; reloads add and remove entries
// on the running cron.
func New(d Drivers) *Runner {
	r := &Runner{d: d, cron: cron.New()}
	r.cron.Start()
	return r
}

func (r *Runner) Stop() { <-r.cron.Stop().Done() }

// Boot loads the table and returns why it could not: nothing serves yet.
func (r *Runner) Boot(ctx context.Context) error { return r.load(ctx) }

// Reload after a write that changed a schedule. It logs rather than fails:
// the row is committed, the worst case is one stale entry until next time.
func (r *Runner) Reload(ctx context.Context) {
	if err := r.load(ctx); err != nil {
		slog.Error("reloading schedules failed", "error", err)
	}
}

// load rebuilds every entry under the mutex, never diffs. An expression
// that will not parse is skipped: the save-time check is the guard.
func (r *Runner) load(ctx context.Context) error {
	scheds, err := r.d.Schedules(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range r.entries {
		r.cron.Remove(id)
	}
	r.entries = nil
	var bad []error
	for _, e := range Entries(r.d, scheds) {
		id, aerr := r.cron.AddFunc(e.Spec, func() {
			if err := e.Fire(context.Background()); err != nil {
				slog.Error("scheduled run not queued", "entry", e.Name, "error", err)
			}
		})
		if aerr != nil {
			bad = append(bad, errors.New(e.Name+": "+aerr.Error()))
			continue
		}
		r.entries = append(r.entries, id)
	}
	return errors.Join(append(bad, err)...)
}
