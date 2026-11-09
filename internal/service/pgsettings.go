package service

import (
	"context"
	"maps"
	"slices"

	"github.com/FyrmForge/stackr/internal/service/errs"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/managed"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// Where a setting's value comes from.
const (
	PGSourceDefault  = "default"  // postgres's own
	PGSourceComputed = "computed" // stackr's, from the memory the instance has
	PGSourceYours    = "yours"    // an override the user set
)

// Which list a setting shows in.
const (
	PGGroupMain     = "main"
	PGGroupAdvanced = "advanced"
	PGGroupOther    = "other"
)

// PGSetting is one setting of a postgres instance. Value is what stackr
// writes (an override, else its computed default), else what the running
// instance reports; empty when neither is known.
type PGSetting struct {
	Name    string `json:"name"`
	Group   string `json:"group"`
	Value   string `json:"value"`
	Source  string `json:"source"`
	Pending bool   `json:"needs_restart"` // the running instance has not taken it yet
}

// PGSettings is a postgres instance's settings page.
type PGSettings struct {
	Running  bool        `json:"running"`
	MemoryMB int         `json:"memory_mb"` // the tile's memory limit, 0 = none: postgres shares the node
	Restart  bool        `json:"needs_restart"`
	Risk     bool        `json:"work_mem_risk"` // work_mem x max_connections outgrows the node's memory
	Settings []PGSetting `json:"settings"`
}

// nodeMemMB is the memory of the node the tile's env runs on, 0 = unknown.
func (o *Orchestrator) nodeMemMB(ctx context.Context, t store.Tile) int {
	n, err := o.nodeOfEnvID(ctx, t.EnvironmentID)
	if err != nil {
		return 0
	}
	if f := o.reportedFacts(ctx, n); f != nil {
		return int(f.MemBytes >> 20)
	}
	return 0
}

// PGSettings lists the settings the panel offers, then any other the user
// set, with where each value comes from. The running instance adds the
// values stackr does not set and which ones wait for a restart.
func (o *Orchestrator) PGSettings(ctx context.Context, tileID string) (PGSettings, error) {
	t, m, err := o.instanceOf(ctx, tileID)
	if err != nil {
		return PGSettings{}, err
	}
	if m.Engine != "postgres" {
		return PGSettings{}, errs.Invalidf("engine", "%s has no settings; only postgres does", m.Engine)
	}
	i := mflow.Instance{Settings: m.Settings, MemMB: t.MemLimitMB, NodeMemMB: o.nodeMemMB(ctx, t)}
	computed := mflow.PGEffective(mflow.Instance{MemMB: i.MemMB, NodeMemMB: i.NodeMemMB})
	var names []string
	groups := map[string]string{}
	for g, l := range map[string][]string{PGGroupMain: mflow.PGMain, PGGroupAdvanced: mflow.PGAdvanced} {
		for _, n := range l {
			groups[n] = g
		}
	}
	names = slices.Concat(mflow.PGMain, mflow.PGAdvanced, slices.Sorted(maps.Keys(m.Settings)))
	names = slices.Compact(names)
	live, pending, err := o.engines.LiveSettings(ctx, t, names)
	out := PGSettings{Running: err == nil, MemoryMB: t.MemLimitMB, Restart: pending}
	for _, n := range names {
		s := PGSetting{Name: n, Group: groups[n], Pending: live[n].Pending}
		if s.Group == "" {
			s.Group = PGGroupOther
		}
		switch v, mine := m.Settings[n]; {
		case mine:
			s.Value, s.Source = v, PGSourceYours
		case computed[n] != "":
			s.Value, s.Source = computed[n], PGSourceComputed
		default:
			s.Value, s.Source = live[n].Value, PGSourceDefault
		}
		out.Settings = append(out.Settings, s)
	}
	eff := func(n string) string {
		for _, s := range out.Settings {
			if s.Name == n {
				return s.Value
			}
		}
		return ""
	}
	mc := eff("max_connections")
	if mc == "" {
		mc = "100"
	}
	wm := eff("work_mem")
	if wm == "" {
		wm = "4MB"
	}
	out.Risk = mflow.PGWorkMemRisk(wm, mc, i.NodeMemMB)
	return out, nil
}

// SetPGSettings sets and unsets the instance's own settings and applies them
// to the running instance: a reload when every changed setting takes effect
// by one, else the instance restarts (the returned job). An owned name, a
// name postgres does not know and a value it rejects are refused, nothing
// stored. The instance has to be running to be asked.
func (o *Orchestrator) SetPGSettings(
	ctx context.Context,
	tileID string,
	set map[string]string,
	unset []string,
) (PGSettings, *Job, error) {
	t, m, err := o.instanceOf(ctx, tileID)
	if err != nil {
		return PGSettings{}, nil, err
	}
	if m.Engine != "postgres" {
		return PGSettings{}, nil, errs.Invalidf("engine", "%s has no settings; only postgres does", m.Engine)
	}
	if err := mflow.CheckSettings(set); err != nil {
		return PGSettings{}, nil, errs.Invalidf("settings", "%s", err.Error())
	}
	for _, n := range unset {
		if err := mflow.CheckSettingName(n); err != nil {
			return PGSettings{}, nil, errs.Invalidf("settings", "%s", err.Error())
		}
	}
	cur := maps.Clone(m.Settings)
	if cur == nil {
		cur = map[string]string{}
	}
	maps.Copy(cur, set)
	for _, n := range unset {
		delete(cur, n)
	}
	changed, removed := mflow.ChangedSettings(m.Settings, cur)
	if len(changed) == 0 {
		v, err := o.PGSettings(ctx, tileID)
		return v, nil, err
	}
	restart, err := o.engines.ApplySettings(ctx, t, cur, changed, removed)
	if err != nil {
		return PGSettings{}, nil, err
	}
	if _, err := o.managed.SetSettings(ctx, m, cur); err != nil {
		return PGSettings{}, nil, err
	}
	var j *Job
	if restart {
		job, err := o.deployNoted(ctx, t.ID, "settings changed: restarting for the ones that need it")
		if err != nil {
			return PGSettings{}, nil, err
		}
		j = &job
	}
	v, err := o.PGSettings(ctx, tileID)
	return v, j, err
}
