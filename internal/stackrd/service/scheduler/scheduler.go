// Package scheduler owns re-registering the two cron tables after anything
// that changes them. Before this existed, 28 call sites each carried their own
// nil guard and their own error policy (one returned 500, fourteen discarded,
// two warned), and four paths that cascade schedule rows forgot to reload at
// all — so a deleted stack's cron kept ticking against a missing tile.
package scheduler

import (
	"context"
	"log/slog"

	"github.com/FyrmForge/stackr/internal/stackrd/infra/backup"
	"github.com/FyrmForge/stackr/internal/stackrd/infra/jobs"
)

// Service reloads the cron and backup schedules. Both halves are optional:
// tests and the API router run without them, which is why the nil guard lives
// here instead of at every caller.
type Service struct {
	jobs    *jobs.Service
	backups *backup.Service
}

// New builds the service. Either dependency may be nil.
func New(j *jobs.Service, b *backup.Service) *Service {
	return &Service{jobs: j, backups: b}
}

// ReloadCron re-registers cron entries for every enabled job.
//
// It returns nothing on purpose. A reload failure must not fail the write that
// preceded it: the row is already committed, and the worst case is one stale
// entry that ticks and logs until the next reload. Callers that want the error
// are the boot path, which uses Boot.
func (s *Service) ReloadCron(ctx context.Context) {
	if s == nil || s.jobs == nil {
		return
	}
	if err := s.jobs.LoadSchedules(ctx); err != nil {
		slog.Error("reloading cron schedules failed", "error", err)
	}
}

// ReloadBackups re-registers cron entries for every enabled backup. Same error
// policy as ReloadCron.
func (s *Service) ReloadBackups(ctx context.Context) {
	if s == nil || s.backups == nil {
		return
	}
	if err := s.backups.LoadSchedules(ctx); err != nil {
		slog.Error("reloading backup schedules failed", "error", err)
	}
}

// Reload does both. Every path that deletes a tile, an environment or a stack
// cascades rows out of both tables, so both need re-registering.
func (s *Service) Reload(ctx context.Context) {
	s.ReloadCron(ctx)
	s.ReloadBackups(ctx)
}

// Boot is the one caller that gets the errors: nothing is serving yet, so a
// schedule table that will not load is worth reporting rather than swallowing.
func (s *Service) Boot(ctx context.Context) (cronErr, backupErr error) {
	if s.jobs != nil {
		cronErr = s.jobs.LoadSchedules(ctx)
	}
	if s.backups != nil {
		backupErr = s.backups.LoadSchedules(ctx)
	}
	return cronErr, backupErr
}
