package service

import (
	"math"
	"testing"
	"time"

	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/metrics"
)

func pgVal(ps []metrics.Point, name string) (float64, bool) {
	for _, p := range ps {
		if p.Name == name {
			return p.V, true
		}
	}
	return 0, false
}

// Counters become per-second rates between two samples; the cache hit ratio
// is of the interval; a counter that went backwards yields no rate.
func TestPGPointsRates(t *testing.T) {
	t0 := time.Unix(1000, 0)
	a := mflow.PGSample{At: t0, ConnUsed: 3, ConnMax: 100,
		DBs: []mflow.PGDatabase{{Name: "app", Bytes: 10, Commits: 100, Rollbacks: 10, BlksHit: 900, BlksRead: 100, Deadlk: 0}}}
	b := a
	b.At = t0.Add(10 * time.Second)
	b.DBs = []mflow.PGDatabase{{Name: "app", Bytes: 20, Commits: 200, Rollbacks: 30, BlksHit: 1700, BlksRead: 200, Deadlk: 5}}
	b.Replicas = []mflow.PGReplica{{Name: "r 1", Bytes: 7, Seconds: 3}}
	ps := pgPoints("pg:x", &a, b, 512)
	want := map[string]float64{"commits_s": 10, "rollbacks_s": 2, "deadlocks_s": 0.5, "cache_hit": 800.0 / 900,
		"db_app": 20, "repl_bytes_r_1": 7, "repl_s_r_1": 3, "archive_bytes": 512, "conn_max": 100}
	for n, w := range want {
		if v, ok := pgVal(ps, n); !ok || math.Abs(v-w) > 1e-9 {
			t.Errorf("%s = %v (%v), want %v", n, v, ok, w)
		}
	}
	if ps := pgPoints("pg:x", nil, b, -1); len(ps) == 0 {
		t.Error("first sample wrote nothing")
	} else if _, ok := pgVal(ps, "commits_s"); ok {
		t.Error("first sample has a rate")
	} else if _, ok := pgVal(ps, "archive_bytes"); ok {
		t.Error("not archiving, yet archive_bytes written")
	}
	back := a
	back.At = b.At
	back.DBs = []mflow.PGDatabase{{Name: "app", Commits: 1}}
	if _, ok := pgVal(pgPoints("pg:x", &b, back, -1), "commits_s"); ok {
		t.Error("a counter reset gave a rate")
	}
}

// Each alert fires past its threshold and not at it.
func TestPGAlerts(t *testing.T) {
	ok := mflow.PGSample{ConnUsed: 80, ConnMax: 100, OldestXactS: 900, SlotKeepMB: 100,
		Replicas: []mflow.PGReplica{{Name: "r", Seconds: 300}},
		Slots:    []mflow.PGSlot{{Name: "s", Bytes: 49 << 20}}}
	if got := pgAlerts("pg", ok); len(got) != 0 {
		t.Errorf("at the thresholds: %+v", got)
	}
	bad := ok
	bad.ConnUsed = 81
	bad.OldestXactS = 901
	bad.Replicas = []mflow.PGReplica{{Name: "r", Seconds: 301}}
	bad.Slots = []mflow.PGSlot{{Name: "s", Bytes: 50 << 20}, {Name: "s2", Bytes: 80 << 20}}
	got := pgAlerts("pg", bad)
	for _, k := range pgAlertKinds {
		if got[k].Sev == "" || got[k].Msg == "" {
			t.Errorf("%s did not fire: %+v", k, got[k])
		}
	}
	if got[AlertPGSlot].Sev != SevCritical {
		t.Errorf("slot at 80%% = %s, want critical", got[AlertPGSlot].Sev)
	}
	warn := ok
	warn.Slots = []mflow.PGSlot{{Name: "s", Bytes: 50 << 20}}
	if got := pgAlerts("pg", warn)[AlertPGSlot]; got.Sev != SevWarning {
		t.Errorf("slot at 50%% = %+v, want warning", got)
	}
	warn.SlotKeepMB = -1 // no cap: nothing to be near
	if got := pgAlerts("pg", warn); got[AlertPGSlot].Sev != "" {
		t.Errorf("uncapped slot alerted: %+v", got)
	}
}
