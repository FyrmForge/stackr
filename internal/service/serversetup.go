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
	keyServerSetupStep = "server_setup_step"
)

// ServerSetup is where the wizard stands.
type ServerSetup struct {
	Done    bool
	Mode    string // "" | "ui" | "config"
	Step    string // the by-hand screen the admin was last on; "" = none yet
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
	if s.Step, _, err = o.store.Settings.Get(ctx, keyServerSetupStep); err != nil {
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

// SetServerSetupStep records the by-hand screen the admin is on, so a reload
// or a return resumes there. An unknown step is ignored.
func (o *Orchestrator) SetServerSetupStep(ctx context.Context, step string) error {
	if !slices.Contains(ServerSetupSteps, step) {
		return nil
	}
	return o.store.Settings.Set(ctx, keyServerSetupStep, step)
}

// FillServerGap saves the secrets a server plan waits on and plans the bound
// file again so the gaps close. With no bound file it only saves and returns
// the zero plan.
func (o *Orchestrator) FillServerGap(ctx context.Context, es []ParamEntry) (ServerPlan, error) {
	if _, err := o.SetParams(ctx, ServerParamScope, es); err != nil {
		return ServerPlan{}, err
	}
	if b, err := o.ServerConfigBinding(ctx); err != nil || b.Repo == "" {
		return ServerPlan{}, err
	}
	return o.PlanServerConfig(ctx)
}

// ServerSetupSteps are the by-hand branch's screens, in order.
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
	switch failing, _ := o.store.Settings.ListPrefix(ctx, problemPrefix+"certfail."); {
	case o.cfg.TLSOff:
		certs.State, certs.Detail = "off", "HTTPS is off on this install"
	case l.State == "skipped" && len(o.managedDomains(ctx)) == 0:
		certs.State, certs.Detail = "skipped", "no domain yet"
	case len(failing) > 0:
		certs.State, certs.Detail = "failing", plural(len(failing), "certificate")+" not issued"
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

// FinishServerSetup stamps server_setup_done_at and drops the resume step.
func (o *Orchestrator) FinishServerSetup(ctx context.Context, by *Principal) error {
	if err := o.store.Settings.Delete(ctx, keyServerSetupStep); err != nil {
		return err
	}
	return o.store.Settings.Set(ctx, keyServerSetupDone, time.Now().UTC().Format(time.RFC3339))
}
