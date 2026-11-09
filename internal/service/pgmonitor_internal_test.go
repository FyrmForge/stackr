package service

import (
	"math"
	"strings"
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
		DBs: []mflow.PGDatabase{{OID: 1, Name: "app", Bytes: 10, Commits: 100, Rollbacks: 10, BlksHit: 900, BlksRead: 100, Deadlk: 0}}}
	b := a
	b.At = t0.Add(10 * time.Second)
	b.DBs = []mflow.PGDatabase{{OID: 1, Name: "app", Bytes: 20, Commits: 200, Rollbacks: 30, BlksHit: 1700, BlksRead: 200, Deadlk: 5}}
	b.Replicas = []mflow.PGReplica{{Name: "r 1", Bytes: 7, Seconds: 3}}
	ps := pgPoints("pg:x", &a, b, 512)
	want := map[string]float64{"commits_s": 10, "rollbacks_s": 2, "deadlocks_s": 0.5, "cache_hit": 800.0 / 900,
		"db_" + pgSeries("app"): 20, "repl_bytes_" + pgSeries("r 1"): 7, "repl_s_" + pgSeries("r 1"): 3, "archive_bytes": 512, "conn_max": 100}
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
	back.DBs = []mflow.PGDatabase{{OID: 1, Name: "app", Commits: 1}}
	if _, ok := pgVal(pgPoints("pg:x", &b, back, -1), "commits_s"); ok {
		t.Error("a counter reset gave a rate")
	}
}

// Each alert fires past its threshold and not at it.
func TestPGAlerts(t *testing.T) {
	ok := mflow.PGSample{ConnUsed: 80, ConnMax: 100, OldestXactS: 900, SlotKeepMB: 100,
		Replicas: []mflow.PGReplica{{Name: "r", Seconds: 30}},
		Slots:    []mflow.PGSlot{{Name: "s", Bytes: 49 << 20}}}
	if got := pgAlerts("pg", ok, nil); len(got) != 0 {
		t.Errorf("at the thresholds: %+v", got)
	}
	bad := ok
	bad.ConnUsed = 81
	bad.OldestXactS = 901
	bad.Replicas = []mflow.PGReplica{{Name: "r", Seconds: 31}}
	bad.Slots = []mflow.PGSlot{{Name: "s", Bytes: 50 << 20}, {Name: "s2", Bytes: 80 << 20}}
	got := pgAlerts("pg", bad, nil)
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
	if got := pgAlerts("pg", warn, nil)[AlertPGSlot]; got.Sev != SevWarning {
		t.Errorf("slot at 50%% = %+v, want warning", got)
	}
	warn.SlotKeepMB = -1 // no cap: nothing to be near
	if got := pgAlerts("pg", warn, nil); got[AlertPGSlot].Sev != "" {
		t.Errorf("uncapped slot alerted: %+v", got)
	}
}

// A reset database or a new one is a new baseline for itself and does not
// poison the others' rates.
func TestPGPointsPerDatabaseDeltas(t *testing.T) {
	t0 := time.Unix(1000, 0)
	a := mflow.PGSample{At: t0, DBs: []mflow.PGDatabase{
		{OID: 1, Commits: 100, BlksHit: 10, BlksRead: 10}, {OID: 2, Commits: 5000}}}
	b := mflow.PGSample{At: t0.Add(10 * time.Second), DBs: []mflow.PGDatabase{
		{OID: 1, Commits: 200, BlksHit: 40, BlksRead: 10}, {OID: 2, Commits: 3}, {OID: 3, Commits: 900}}}
	ps := pgPoints("pg:x", &a, b, -1)
	if v, ok := pgVal(ps, "commits_s"); !ok || v != 10 {
		t.Errorf("commits_s = %v (%v), want 10 from the one steady database", v, ok)
	}
	if v, _ := pgVal(ps, "cache_hit"); v != 1 {
		t.Errorf("cache_hit = %v, want 1", v)
	}
	only := mflow.PGSample{At: b.At, DBs: []mflow.PGDatabase{{OID: 9}}}
	if _, ok := pgVal(pgPoints("pg:x", &a, only, -1), "commits_s"); ok {
		t.Error("no database to compare gave a rate")
	}
}

// Two names that sanitise alike stay two series; the label keeps the name.
func TestPGSeriesCollisions(t *testing.T) {
	if pgSeries("east-1") == pgSeries("east 1") || pgSeries("east-1") != pgSeries("east-1") {
		t.Error("collision or unstable")
	}
	l := pgLine("a,b\nc")
	if l.Label != "a b c" || strings.ContainsAny(l.Key, ", \n") {
		t.Errorf("line = %+v", l)
	}
}

// An open problem clears only below its lower mark.
func TestPGAlertsHysteresis(t *testing.T) {
	s := mflow.PGSample{ConnUsed: 78, ConnMax: 100, OldestXactS: 14 * 60, SlotKeepMB: 100,
		Replicas: []mflow.PGReplica{{Name: "r", Seconds: 25}},
		Slots:    []mflow.PGSlot{{Name: "s", Bytes: 47 << 20}}}
	if got := pgAlerts("pg", s, nil); len(got) != 0 {
		t.Errorf("closed, under thresholds: %+v", got)
	}
	was := map[string]string{AlertPGConn: SevWarning, AlertPGLag: SevWarning, AlertPGXact: SevWarning, AlertPGSlot: SevWarning}
	if got := pgAlerts("pg", s, was); len(got) != 4 {
		t.Errorf("open, above clear marks: %+v", got)
	}
	s.ConnUsed, s.OldestXactS, s.Replicas[0].Seconds, s.Slots[0].Bytes = 74, 12*60, 15, 44<<20
	if got := pgAlerts("pg", s, was); len(got) != 0 {
		t.Errorf("open, under clear marks: %+v", got)
	}
	s.Slots[0].Bytes = 78 << 20 // was critical: stays critical above 75%, drops to warning under
	if got := pgAlerts("pg", s, map[string]string{AlertPGSlot: SevCritical})[AlertPGSlot]; got.Sev != SevCritical {
		t.Errorf("critical slot at 78%% = %+v", got)
	}
	if got := pgAlerts("pg", s, map[string]string{AlertPGSlot: SevWarning})[AlertPGSlot]; got.Sev != SevWarning {
		t.Errorf("warning slot at 78%% = %+v", got)
	}
}
