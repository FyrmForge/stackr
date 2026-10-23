package service

import (
	"context"
	"slices"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/connector"
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
	"ui":     ServerSetupSteps,
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

// ServerSetupSteps are the by-hand branch's screens, in order. The URL is
// the position: nothing records how far an admin got.
var ServerSetupSteps = []string{"domains", "certificates", ServerSetupGit, "observability", "backups", "nodes", "summary"}

// ServerSetupGit is the step that connects a git host for every org.
const ServerSetupGit = "git"

// SetupLine is one summary row: State is "on", "off" or "skipped", read
// from what is live now (skipped = nothing set up; off = set up, switched off).
type SetupLine struct {
	Step, State, Detail string
}

// ServerSetupSummary is the wizard's last screen, one line per step that
// sets something.
func (o *Orchestrator) ServerSetupSummary(ctx context.Context) ([]SetupLine, error) {
	var out []SetupLine
	panel, err := o.settings.Get(ctx, "panel_domain")
	if err != nil {
		return nil, err
	}
	root, err := o.settings.Get(ctx, "root_domain")
	if err != nil {
		return nil, err
	}
	l := SetupLine{Step: "domains", State: "skipped"}
	if root == panel {
		root = ""
	}
	if panel != "" || root != "" {
		l.State, l.Detail = "on", strings.Trim(panel+", "+root, ", ")
	}
	certs := SetupLine{Step: "certificates", State: "on", Detail: "one per domain, automatic"}
	if ms := o.managedDomains(ctx); len(ms) > 0 {
		certs.Detail = "wildcards and DNS records for " + strings.Join(ms, ", ")
	}
	out = append(out, l, certs)

	cs, err := o.conns.ListServer(ctx)
	if err != nil {
		return nil, err
	}
	l = SetupLine{Step: ServerSetupGit, State: "skipped"}
	// one its provider never confirmed is not set up
	if n := len(slices.DeleteFunc(cs, func(c Connector) bool { return !connector.Connected(c) })); n > 0 {
		l.State, l.Detail = "on", plural(n, "connector")
	}
	out = append(out, l)

	ob, err := o.settings.Get(ctx, "observe_enabled")
	if err != nil {
		return nil, err
	}
	l = SetupLine{Step: "observability", State: "off", Detail: "24 hours of live graphs"}
	if ob == "true" {
		l.State, l.Detail = "on", "history and log search"
	}
	out = append(out, l)

	ds, err := o.backups.Global(ctx)
	if err != nil {
		return nil, err
	}
	l = SetupLine{Step: "backups", State: "skipped", Detail: "local only"}
	if n := len(slices.DeleteFunc(ds, func(d BackupDest) bool { return d.Kind == "local" })); n > 0 {
		l.State, l.Detail = "on", plural(n, "destination")
	}
	pb, err := o.settings.Get(ctx, "panel_backup_enabled")
	if err != nil {
		return nil, err
	}
	if pb != "true" {
		l.Detail += ", panel backup off"
	}
	out = append(out, l)

	ns, err := o.Nodes(ctx)
	if err != nil {
		return nil, err
	}
	out = append(out, SetupLine{Step: "nodes", State: "on", Detail: plural(len(ns), "node")})
	return out, nil
}

// FinishServerSetup stamps server_setup_done_at.
func (o *Orchestrator) FinishServerSetup(ctx context.Context, by *Principal) error {
	return o.store.Settings.Set(ctx, keyServerSetupDone, time.Now().UTC().Format(time.RFC3339))
}
