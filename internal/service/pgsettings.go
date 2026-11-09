package service

import (
	"context"
	"log/slog"
	"maps"
	"slices"
	"sync"

	"github.com/FyrmForge/stackr/internal/service/errs"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
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

// nodeDiskMB is the free disk of the node the tile's env runs on, 0 = unknown.
func (o *Orchestrator) nodeDiskMB(ctx context.Context, t store.Tile) int {
	n, err := o.nodeOfEnvID(ctx, t.EnvironmentID)
	if err != nil {
		return 0
	}
	if f := o.reportedFacts(ctx, n); f != nil {
		return int(f.DiskFree >> 20)
	}
	return 0
}

// nodeDiskTotalMB is the whole disk of the node the tile's env runs on, 0 =
// unknown.
func (o *Orchestrator) nodeDiskTotalMB(ctx context.Context, t store.Tile) int {
	n, err := o.nodeOfEnvID(ctx, t.EnvironmentID)
	if err != nil {
		return 0
	}
	if f := o.reportedFacts(ctx, n); f != nil {
		return int(f.DiskTotal >> 20)
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

// pgLock serialises changes to one instance's settings: the row, the file
// and the live config move together or not at all.
func (o *Orchestrator) pgLock(instanceID string) *sync.Mutex {
	m, _ := o.repoLocks.LoadOrStore("pg settings "+instanceID, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// applyPGSettings makes cur the instance's settings: the running instance
// is asked first (unknown names, rejected values), its file written and
// reloaded or flagged for a restart, then the row stored; a store that fails
// puts the old file back. The caller holds pgLock. restart: the caller owes
// the instance a restart.
func (o *Orchestrator) applyPGSettings(ctx context.Context, t Tile, m OperatedInstance, cur map[string]string) (bool, error) {
	changed, removed := mflow.ChangedSettings(m.Settings, cur)
	if len(changed) == 0 {
		return false, nil
	}
	if slices.Contains(changed, "max_connections") {
		// refused before anything is written: the pooler's budget must hold
		// every slice database
		if err := o.engines.CheckPoolBudget(ctx, t, cur, 0); err != nil {
			return false, err
		}
	}
	restart, err := o.engines.ApplySettings(ctx, t, cur, changed, removed)
	if err != nil {
		return false, err
	}
	// every running standby takes the file too (a standby's limits may not
	// be below the primary's); one that refuses puts everything back
	sbs, err := o.applyToStandbys(ctx, t, m.ID, cur, changed, removed)
	if err != nil {
		back, backUnset := mflow.ChangedSettings(cur, m.Settings)
		_, _ = o.engines.ApplySettings(ctx, t, m.Settings, back, backUnset)
		return false, err
	}
	if _, err := o.operated.SetSettings(ctx, m, cur); err != nil {
		back, backUnset := mflow.ChangedSettings(cur, m.Settings)
		_, _ = o.engines.ApplySettings(ctx, t, m.Settings, back, backUnset)
		o.undoStandbys(ctx, t, sbs, m.Settings, back, backUnset)
		return false, err
	}
	if !restart && slices.Contains(changed, "max_connections") {
		// A restart's deploy writes the pool caps once postgres runs with the
		// new value (they follow the live one); only a change that took
		// effect now is synced now.
		if err := o.engines.SyncPooler(ctx, t); err != nil {
			slog.Warn("sync pooler", "tile", t.ID, "err", err)
		}
	}
	return restart, nil
}

// settingsFromFile applies a stack file's settings to an existing instance
// that is running, with the checks SetPGSettings makes, before the plan
// stores them: applied is true once they are stored. A stopped instance is
// not asked (applied false): its next start reads them, and a name postgres
// does not know shows then as a failed start.
func (o *Orchestrator) settingsFromFile(ctx context.Context, t Tile, m OperatedInstance, s map[string]string) (bool, error) {
	mu := o.pgLock(m.ID)
	mu.Lock()
	defer mu.Unlock()
	if _, err := o.applyPGSettings(ctx, t, m, s); err != nil {
		if _, stopped := errs.IsConflict(err); stopped {
			return false, nil
		}
		return false, err
	}
	return true, nil
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
	// the row is read again under the lock: a change that ran first is seen
	mu := o.pgLock(m.ID)
	mu.Lock()
	defer mu.Unlock()
	if t, m, err = o.instanceOf(ctx, tileID); err != nil {
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
	restart, err := o.applyPGSettings(ctx, t, m, cur)
	if err != nil {
		return PGSettings{}, nil, err
	}
	var j *Job
	if restart {
		// standbys before the primary where the limits allow: those that may
		// restart now do, the rest wait for the primary (syncStandbyConf)
		o.syncStandbyConf(ctx, t)
		job, err := o.deployNoted(ctx, t.ID, "settings changed: restarting for the ones that need it")
		if err != nil {
			return PGSettings{}, nil, err
		}
		j = &job
	}
	v, err := o.PGSettings(ctx, tileID)
	return v, j, err
}
