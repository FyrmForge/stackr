// Package schedule is the cron runner: backup schedules, cron tiles, the
// daily orphan retention job, the image-watch tick and the traffic sample,
// on one robfig/cron. A tick enqueues through the func it was given and
// returns; it never runs the work, except the traffic sample, which is a
// read and runs inline. A missed tick is missed: nothing catches up after a
// restart.
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
	// Crons lists the cron tiles in a deployed release; Cron queues a run
	// of one. A paused tile gets no entry.
	Crons func(ctx context.Context) ([]store.Tile, error)
	Cron  func(ctx context.Context, t store.Tile) error
	// Traffic samples conntrack every 5 s (flow/traffic); nil = no entry.
	Traffic func(ctx context.Context) error
}

type Entry struct {
	Name, Spec string
	Fire       func(ctx context.Context) error
}

// Entries maps the rows to cron entries: the pure half, tested alone.
func Entries(d Drivers, scheds []store.BackupSchedule, crons []store.Tile) []Entry {
	out := []Entry{
		{Name: "orphans", Spec: "@daily", Fire: d.Orphans},
		{Name: "image-watch", Spec: "@every 1m", Fire: d.Watch},
	}
	if d.Traffic != nil {
		out = append(out, Entry{Name: "traffic", Spec: "@every 5s", Fire: d.Traffic})
	}
	for _, s := range scheds {
		spec := s.Cron
		if s.Timezone != "" {
			spec = "CRON_TZ=" + s.Timezone + " " + s.Cron
		}
		out = append(out, Entry{Name: "backup " + s.ID, Spec: spec,
			Fire: func(ctx context.Context) error { return d.Backup(ctx, s) }})
	}
	for _, t := range crons {
		if t.Paused {
			continue
		}
		out = append(out, Entry{Name: "cron " + t.ID, Spec: t.Schedule,
			Fire: func(ctx context.Context) error { return d.Cron(ctx, t) }})
	}
	return out
}

type Runner struct {
	d       Drivers
	mu      sync.Mutex
	cron    *cron.Cron
	entries []cron.EntryID
	names   []string
}

// New starts the cron once for the process; reloads add and remove entries
// on the running cron.
func New(d Drivers) *Runner {
	r := &Runner{d: d, cron: cron.New()}
	r.cron.Start()
	return r
}

func (r *Runner) Stop() { <-r.cron.Stop().Done() }

// Names are the loaded entries' names ("backup <id>", "cron <tile id>", …).
func (r *Runner) Names() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.names...)
}

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
	var crons []store.Tile
	if r.d.Crons != nil {
		var cerr error
		crons, cerr = r.d.Crons(ctx)
		err = errors.Join(err, cerr)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, id := range r.entries {
		r.cron.Remove(id)
	}
	r.entries, r.names = nil, nil
	var bad []error
	for _, e := range Entries(r.d, scheds, crons) {
		id, aerr := r.cron.AddFunc(e.Spec, func() {
			if err := e.Fire(context.Background()); err != nil {
				slog.Error("scheduled run not queued", "entry", e.Name, "error", err)
			}
		})
		if aerr != nil {
			bad = append(bad, errors.New(e.Name+": "+aerr.Error()))
			continue
		}
		r.entries, r.names = append(r.entries, id), append(r.names, e.Name)
	}
	return errors.Join(append(bad, err)...)
}
