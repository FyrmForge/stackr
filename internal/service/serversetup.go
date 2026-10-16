package service

import (
	"context"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
)

// Server setup: the first-run wizard an admin lands on while the
// server_setup_done_at setting is unset, by hand or from a server file.
// The two keys are not in the settings catalogue: they are state, not knobs.
const (
	keyServerSetupDone = "server_setup_done_at"
	keyServerSetupMode = "server_setup_mode"
)

// ServerSetup is where the wizard stands.
type ServerSetup struct {
	Done    bool
	Mode    string // "" | "ui" | "config"
	Steps   []string
	Binding ServerBinding
}

// serverSetupSteps are the wizard's screens per branch.
var serverSetupSteps = map[string][]string{
	"":       {"mode"},
	"ui":     {"domains", "backup", "placements"},
	"config": {"file", "plan"},
}

// ServerSetupState is the wizard's state.
func (o *Orchestrator) ServerSetupState(ctx context.Context) (ServerSetup, error) {
	var s ServerSetup
	done, ok, err := o.store.Settings.Get(ctx, keyServerSetupDone)
	if err != nil {
		return s, err
	}
	s.Done = ok && done != ""
	if s.Mode, _, err = o.store.Settings.Get(ctx, keyServerSetupMode); err != nil {
		return s, err
	}
	s.Steps = serverSetupSteps[s.Mode]
	s.Binding, err = o.ServerConfigBinding(ctx)
	return s, err
}

// SetServerSetupMode picks the wizard's branch.
func (o *Orchestrator) SetServerSetupMode(ctx context.Context, by *Principal, mode string) (ServerSetup, error) {
	if mode != "ui" && mode != "config" {
		return ServerSetup{}, errs.Invalidf("mode", "pick by hand or from a file")
	}
	if err := o.store.Settings.Set(ctx, keyServerSetupMode, mode); err != nil {
		return ServerSetup{}, err
	}
	return o.ServerSetupState(ctx)
}

// FinishServerSetup stamps server_setup_done_at.
func (o *Orchestrator) FinishServerSetup(ctx context.Context, by *Principal) error {
	return o.store.Settings.Set(ctx, keyServerSetupDone, time.Now().UTC().Format(time.RFC3339))
}
