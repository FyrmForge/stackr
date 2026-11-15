package service_test

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

// The one-shot recheck after a panel record write: the write's own check
// skips the lookup and the probe (the record is minutes old), the recheck
// waits out a TTL, and a lookup that fails keeps it due.
func TestPanelRecheckAfterAWrite(t *testing.T) {
	t.Parallel()
	env, _, _ := scopedRig(t)
	setIP(t, env, "")
	var lookups, probes atomic.Int32
	var lookupErr atomic.Bool
	env.Orch.SetPanelSeams(
		func(context.Context, string) (string, string) { return "198.51.100.77", "w1" },
		func(context.Context, string, string) (string, bool) { probes.Add(1); return "", false },
		func(context.Context, string) ([]string, error) {
			lookups.Add(1)
			if lookupErr.Load() {
				return nil, errors.New("SERVFAIL")
			}
			return []string{"198.51.100.77"}, nil
		},
	)
	ctx := context.Background()
	setSettings(t, env, map[string]string{"panel_domain": "docs.stackr.acme.dev"}) // the sync that writes
	if !env.Orch.PanelDue() {
		t.Fatal("a write left no recheck due")
	}
	if probes.Load() != 0 {
		t.Errorf("the write's own check probed %d times", probes.Load())
	}
	base := lookups.Load()         // the sync's own lookups (what the record is held at)
	env.Orch.RecheckPanelName(ctx) // inside the TTL: not yet
	if lookups.Load() != base {
		t.Errorf("the recheck ran before the record's TTL: %d lookups", lookups.Load()-base)
	}
	env.Orch.ForcePanelDue()
	lookupErr.Store(true)
	env.Orch.RecheckPanelName(ctx)
	if lookups.Load() == base || !env.Orch.PanelDue() {
		t.Errorf("a failed lookup cleared the recheck (%d lookups)", lookups.Load()-base)
	}
	lookupErr.Store(false)
	env.Orch.RecheckPanelName(ctx)
	if env.Orch.PanelDue() {
		t.Error("an answered lookup left the recheck due")
	}
	if probes.Load() == 0 {
		t.Error("the judging check never probed")
	}
}
