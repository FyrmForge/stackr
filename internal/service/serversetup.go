package service

import (
	"context"

	"github.com/FyrmForge/stackr/internal/service/errs"
)

// Server setup: the first-run wizard an admin lands on while the
// server_setup_done_at setting is unset, by hand or from a server file.

// ServerSetup is where the wizard stands.
type ServerSetup struct {
	Done    bool
	Mode    string // "" | "ui" | "config"
	Steps   []string
	Binding ServerBinding
}

// ServerSetupState is the wizard's state.
func (o *Orchestrator) ServerSetupState(ctx context.Context) (ServerSetup, error) {
	return ServerSetup{}, errs.ErrNotBuilt
}

// SetServerSetupMode picks the wizard's branch.
func (o *Orchestrator) SetServerSetupMode(ctx context.Context, by *Principal, mode string) (ServerSetup, error) {
	return ServerSetup{}, errs.ErrNotBuilt
}

// FinishServerSetup stamps server_setup_done_at.
func (o *Orchestrator) FinishServerSetup(ctx context.Context, by *Principal) error {
	return errs.ErrNotBuilt
}
