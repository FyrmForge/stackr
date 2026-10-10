package service

import "context"

// SyncGitHubHooks repoints every GitHub App connector's webhook at the
// current panel URL, so Apps made before a panel_domain change keep working.
// A failing connector is logged and skipped; the errors come back joined.
func (o *Orchestrator) SyncGitHubHooks(ctx context.Context) error {
	return o.conns.SyncHooks(ctx)
}
