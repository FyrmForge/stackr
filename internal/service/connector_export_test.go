package service

import (
	"context"
	"io"
)

// SetUpgradeHeld sets the upgrade hold, as a running helper does.
func (o *Orchestrator) SetUpgradeHeld(on bool) { o.upgrading.Store(on) }

// ServerFile is serverFile for the external tests.
func (o *Orchestrator) ServerFile(ctx context.Context, commit string) ([]byte, string, error) {
	return o.serverFile(ctx, commit, io.Discard)
}
