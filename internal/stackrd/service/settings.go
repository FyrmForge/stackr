package service

import (
	"context"
	"net/url"
	"strings"

	"github.com/FyrmForge/stackr/internal/stackrd/config/settings"
	svcproxy "github.com/FyrmForge/stackr/internal/stackrd/service/proxy"
	"github.com/FyrmForge/stackr/internal/stackrd/service/svcerr"
	"github.com/FyrmForge/stackr/internal/stackrd/store/repo"
)

// SettingsService owns every write to the defaults cascade — server, org,
// stack, environment — from both surfaces.
//
// Two things were only true on some of the four paths before. The config-file
// gate was one: `defaults:` is owned by the config file at org
// (orgconf.go:687-700), stack (apply:932-937) and env (apply:955-964) level,
// and all three replace the whole blob, so an ungated write to a managed
// object is a value the next apply silently reverts. Only the panel's org
// page refused it. The other is the proxy resync: the cascade feeds the
// rendered traefik routes through `protect`, which are otherwise only
// rewritten on a domain change or a deploy, so a save that skips it turns
// protection on in the database and nowhere else.
type SettingsService struct {
	store repo.Store
	px    *svcproxy.Service
}

// NewSettingsService creates a new settings service.
func NewSettingsService(store repo.Store, px *svcproxy.Service) *SettingsService {
	return &SettingsService{store: store, px: px}
}

// managedBy is the one refusal, worded the same at every level so a user who
// hits it on a stack reads what they read on an org.
func managedBy(kind, repoName string) error {
	return svcerr.Conflictf("this %s is managed by %s; set defaults: in the config file", kind, repoName)
}

// merge applies the form values on top of what the level has and validates the
// result. A key sent empty clears the override, which is how a level gives a
// knob back to the one above it.
func merge(cur settings.Settings, vals url.Values) (string, error) {
	next := settings.Merge(cur, vals)
	if err := next.Check(); err != nil {
		return "", svcerr.Invalid{Msg: err.Error()}
	}
	return next.JSON(), nil
}

// resync re-renders the proxy after a write. Best effort: the settings are
// saved either way, and a proxy that is down is its own alarm.
func (s *SettingsService) resync(ctx context.Context) {
	if s == nil || s.px == nil {
		return
	}
	_ = s.px.Resync(ctx)
}

// SaveServer writes the installation-wide defaults and the server's display
// name. The name is required: the panel form posts it on every save and wrote
// whatever arrived, so a save from a form that had lost the field left the
// server called "".
func (s *SettingsService) SaveServer(ctx context.Context, sv *repo.Server, name string, vals url.Values) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return svcerr.Invalidf("", "a server name is required")
	}
	blob, err := merge(settings.Parse(sv.Settings), vals)
	if err != nil {
		return err
	}
	sv.Name, sv.Settings = name, blob
	if err := s.store.UpdateServer(ctx, sv); err != nil {
		return err
	}
	s.resync(ctx)
	return nil
}

// SaveOrg writes the org's rung.
func (s *SettingsService) SaveOrg(ctx context.Context, o *repo.Org, vals url.Values) error {
	if o.ConfigManaged() {
		return managedBy("organization", o.ConfigRepo)
	}
	blob, err := merge(settings.Parse(o.Settings), vals)
	if err != nil {
		return err
	}
	o.Settings = blob
	if err := s.store.UpdateOrg(ctx, o); err != nil {
		return err
	}
	s.resync(ctx)
	return nil
}

// SaveStack writes the stack's rung.
func (s *SettingsService) SaveStack(ctx context.Context, st *repo.Stack, vals url.Values) error {
	if st.ConfigManaged() {
		return managedBy("stack", st.ConfigRepo)
	}
	blob, err := merge(settings.Parse(st.Settings), vals)
	if err != nil {
		return err
	}
	st.Settings = blob
	if err := s.store.UpdateStack(ctx, st); err != nil {
		return err
	}
	s.resync(ctx)
	return nil
}

// SaveEnv writes an environment's rung. The gate is the stack's: an
// environment has no config file of its own, it is a block in its stack's.
func (s *SettingsService) SaveEnv(ctx context.Context, env *repo.Environment, vals url.Values) error {
	st, err := s.store.GetStack(ctx, env.StackID)
	if err != nil {
		return err
	}
	if st == nil {
		return svcerr.ErrNotFound
	}
	if st.ConfigManaged() {
		return managedBy("environment's stack", st.ConfigRepo)
	}
	blob, err := merge(settings.Parse(env.Settings), vals)
	if err != nil {
		return err
	}
	env.Settings = blob
	if err := s.store.UpdateEnvironment(ctx, env); err != nil {
		return err
	}
	s.resync(ctx)
	return nil
}
