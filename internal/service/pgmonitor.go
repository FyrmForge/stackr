package service

import (
	"context"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
	"github.com/FyrmForge/stackr/internal/service/internal/metrics"
)

// Database monitoring for operated postgres: each metrics tick runs one psql
// exec per running instance (flow/operated PGStats), writes the numbers into
// the metrics store under pg:<tile id>, and keeps the problems banner honest.
// No exporter: sagr can take the series over by config later.

// Alert events of a postgres instance; the problem key is pgmon.<kind>.<instance id>.
const (
	AlertPGConn = "pgmon.conn" // connections near max_connections
	AlertPGLag  = "pgmon.lag"  // a standby far behind
	AlertPGSlot = "pgmon.slot" // a slot holds WAL near the cap
	AlertPGXact = "pgmon.xact" // a transaction open too long

	pgConnWarn   = 0.8
	pgSlotWarn   = 0.5
	pgSlotCrit   = 0.8
	pgLagWarn    = 5 * time.Minute
	pgXactWarn   = 15 * time.Minute
	pgStatsLimit = 10 * time.Second
	pgRoundLimit = 20 * time.Second // the whole round, under the 30 s tick

	// Hysteresis: an open problem clears only once past these.
	pgConnClear     = 0.75
	pgSlotClear     = 0.45 // of the cap, under the warning
	pgSlotCritClear = 0.75
	pgLagClear      = 4 * time.Minute
	pgXactClear     = 13 * time.Minute
)

var pgAlertKinds = []string{AlertPGConn, AlertPGLag, AlertPGSlot, AlertPGXact}

func pgKey(tileID string) string { return "pg:" + tileID }

var pgBadName = regexp.MustCompile(`[^A-Za-z0-9_]`)

// pgSeries makes a database, standby or slot name safe in a series name. A
// short hash of the original keeps "east-1" and "east 1" apart.
func pgSeries(name string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(name))
	return fmt.Sprintf("%s_%06x", pgBadName.ReplaceAllString(name, "_"), h.Sum32()&0xffffff)
}

// PGLine is one line of a chart: the series suffix and what to call it.
type PGLine struct{ Key, Label string }

func pgLine(name string) PGLine {
	return PGLine{Key: pgSeries(name), Label: strings.NewReplacer(",", " ", "\n", " ", "\r", " ").Replace(name)}
}

// pgPoints turns a sample (and the one before it, for the counters) into
// points. A counter that went backwards (stats reset, a database dropped)
// yields no rate that round. archiveBytes < 0 = not archiving.
func pgPoints(key string, prev *mflow.PGSample, cur mflow.PGSample, archiveBytes int64) []metrics.Point {
	var out []metrics.Point
	add := func(name string, v float64) { out = append(out, metrics.Point{Key: key, Name: name, T: cur.At, V: v}) }
	add("conn_used", float64(cur.ConnUsed))
	add("conn_max", float64(cur.ConnMax))
	add("conn_active", float64(cur.ConnActive))
	add("conn_idle", float64(cur.ConnIdle))
	add("conn_idle_tx", float64(cur.ConnIdleTx))
	add("xact_oldest_s", float64(cur.OldestXactS))
	for _, d := range cur.DBs {
		add("db_"+pgSeries(d.Name), float64(d.Bytes))
	}
	for _, r := range cur.Replicas {
		add("repl_bytes_"+pgSeries(r.Name), float64(r.Bytes))
		add("repl_s_"+pgSeries(r.Name), float64(r.Seconds))
	}
	for _, s := range cur.Slots {
		add("slot_"+pgSeries(s.Name), float64(s.Bytes))
	}
	if archiveBytes >= 0 {
		add("archive_bytes", float64(archiveBytes))
	}
	if prev == nil {
		return out
	}
	dt := cur.At.Sub(prev.At).Seconds()
	if dt <= 0 {
		return out
	}
	// per database by oid: a reset counter or a new database is a new
	// baseline and leaves that database out of the sums
	before := map[int64]mflow.PGDatabase{}
	for _, d := range prev.DBs {
		before[d.OID] = d
	}
	var commit, rollback, hit, read, dead int64
	valid := 0
	for _, d := range cur.DBs {
		p, ok := before[d.OID]
		if !ok || d.Commits < p.Commits || d.Rollbacks < p.Rollbacks || d.BlksHit < p.BlksHit ||
			d.BlksRead < p.BlksRead || d.Deadlk < p.Deadlk {
			continue
		}
		valid++
		commit, rollback, hit = commit+d.Commits-p.Commits, rollback+d.Rollbacks-p.Rollbacks, hit+d.BlksHit-p.BlksHit
		read, dead = read+d.BlksRead-p.BlksRead, dead+d.Deadlk-p.Deadlk
	}
	if valid == 0 {
		return out
	}
	add("commits_s", float64(commit)/dt)
	add("rollbacks_s", float64(rollback)/dt)
	add("deadlocks_s", float64(dead)/dt)
	if hit+read > 0 {
		add("cache_hit", float64(hit)/float64(hit+read))
	}
	return out
}

// pgProblem is one alert's verdict; Sev "" = healthy.
type pgProblem struct{ Sev, Msg string }

// pgAlerts judges a sample, one verdict per alert kind. was is the severity
// each kind has open now ("" = none): an open problem needs the lower clear
// mark, so a value hovering at a threshold does not flap.
func pgAlerts(slug string, s mflow.PGSample, was map[string]string) map[string]pgProblem {
	out := map[string]pgProblem{}
	conn := float64(s.ConnUsed) / float64(max(s.ConnMax, 1))
	if conn > pgConnWarn || (was[AlertPGConn] != "" && conn > pgConnClear) {
		out[AlertPGConn] = pgProblem{SevWarning, fmt.Sprintf("%s: %d of %d connections are in use.", slug, s.ConnUsed, s.ConnMax)}
	}
	for _, r := range s.Replicas {
		lag := time.Duration(r.Seconds) * time.Second
		if lag > pgLagWarn || (was[AlertPGLag] != "" && lag > pgLagClear) {
			out[AlertPGLag] = pgProblem{SevWarning, fmt.Sprintf("%s: standby %s is %s behind.", slug, r.Name, agoWords(lag))}
			break
		}
	}
	if s.SlotKeepMB > 0 {
		limit := float64(s.SlotKeepMB) * (1 << 20)
		for _, sl := range s.Slots {
			frac := float64(sl.Bytes) / limit
			var sev string
			switch w := was[AlertPGSlot]; {
			case frac >= pgSlotCrit || (w == SevCritical && frac >= pgSlotCritClear):
				sev = SevCritical
			case frac >= pgSlotWarn || (w != "" && frac >= pgSlotClear):
				sev = SevWarning
			default:
				continue
			}
			if cur, ok := out[AlertPGSlot]; !ok || (cur.Sev == SevWarning && sev == SevCritical) {
				out[AlertPGSlot] = pgProblem{sev, fmt.Sprintf("%s: replication slot %s holds %d%% of the WAL cap.", slug, sl.Name, int(frac*100))}
			}
		}
	}
	if d := time.Duration(s.OldestXactS) * time.Second; d > pgXactWarn || (was[AlertPGXact] != "" && d > pgXactClear) {
		out[AlertPGXact] = pgProblem{SevWarning, fmt.Sprintf("%s: a transaction has been open for %s.", slug, agoWords(d))}
	}
	return out
}

// pgOpenSev is the severity of the problem a kind has open on an instance.
func (o *Orchestrator) pgOpenSev(ctx context.Context, kind, instanceID string) string {
	v, ok, err := o.store.Settings.Get(ctx, problemKey("alert."+kind+"."+instanceID))
	if err != nil || !ok {
		return ""
	}
	var r problemRow
	if json.Unmarshal([]byte(v), &r) != nil {
		return ""
	}
	return r.Severity
}

// pgSnap is what the panel remembers of an instance: its last sample.
type pgSnap struct{ last mflow.PGSample }

// pgTick samples every postgres instance that answers, writes the points and
// the alerts, all within one deadline. A stopped or absent container is a
// conclusive "not running": its alerts clear. A failed read changes nothing.
func (o *Orchestrator) pgTick(ctx context.Context) []metrics.Point {
	ms, err := o.operated.WithEngine(ctx, "postgres")
	if err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, pgRoundLimit)
	defer cancel()
	var (
		mu  sync.Mutex
		pts []metrics.Point
		wg  sync.WaitGroup
	)
	live := map[string]bool{}
	sem := make(chan struct{}, 4)
	for _, m := range ms {
		live[m.ID] = true
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			cctx, cancel := context.WithTimeout(ctx, pgStatsLimit)
			defer cancel()
			t, err := o.tiles.Get(cctx, m.TileID)
			if err != nil {
				return
			}
			if cs, err := o.tiles.Replicas(cctx, t); err == nil && !slices.ContainsFunc(cs, func(c docker.Container) bool { return c.State == "running" }) {
				o.pgClear(ctx, m.ID)
				o.metrics.mu.Lock()
				delete(o.metrics.pg, m.TileID)
				o.metrics.mu.Unlock()
				return
			}
			cur, err := o.engines.PGStats(cctx, t)
			if err != nil {
				return
			}
			o.metrics.mu.Lock()
			if o.metrics.pg == nil {
				o.metrics.pg = map[string]pgSnap{}
			}
			var prev *mflow.PGSample
			if p, ok := o.metrics.pg[m.TileID]; ok {
				prev = &p.last
			}
			o.metrics.pg[m.TileID] = pgSnap{cur}
			o.metrics.mu.Unlock()
			arch := int64(-1)
			if st, ok := o.pitrState(cctx, m.ID); ok && st.ArchiverSeen && m.PITRDays > 0 {
				arch = st.DirBytes
			}
			ps := pgPoints(pgKey(m.TileID), prev, cur, arch)
			was := map[string]string{}
			for _, kind := range pgAlertKinds {
				was[kind] = o.pgOpenSev(ctx, kind, m.ID)
			}
			verdicts := pgAlerts(t.Slug, cur, was)
			for _, kind := range pgAlertKinds {
				v := verdicts[kind]
				o.pitrProblem(ctx, "alert."+kind+"."+m.ID, kind, v.Sev, t.Slug, v.Msg)
			}
			mu.Lock()
			pts = append(pts, ps...)
			mu.Unlock()
		}()
	}
	wg.Wait()
	for _, kind := range pgAlertKinds {
		o.sweep(ctx, kind+".", func(rest string) bool { return live[rest] })
	}
	o.metrics.mu.Lock()
	for id := range o.metrics.pg {
		if !slices.ContainsFunc(ms, func(m OperatedInstance) bool { return m.TileID == id }) {
			delete(o.metrics.pg, id)
		}
	}
	o.metrics.mu.Unlock()
	return pts
}

// pgClear closes every monitoring problem of an instance.
func (o *Orchestrator) pgClear(ctx context.Context, instanceID string) {
	for _, kind := range pgAlertKinds {
		o.pitrProblem(ctx, "alert."+kind+"."+instanceID, kind, "", "", "")
	}
}

// PGQuery is one of the instance's costliest statements.
type PGQuery = mflow.PGQuery

// PGMonitor is the Database tab: the newest sample's headline and the names
// of the series that have lines to draw, plus the top statements read when
// the tab opens.
type PGMonitor struct {
	Sampled     bool
	At          time.Time
	ConnUsed    int
	ConnMax     int
	OldestXactS int64
	Archiving   bool
	DBs         []PGLine
	Replicas    []PGLine
	Slots       []PGLine
	Top         []PGQuery
	TopError    string // why Top is empty when it is not just quiet
}

func (o *Orchestrator) pgInstance(ctx context.Context, tileID string) (Tile, OperatedInstance, error) {
	t, m, err := o.instanceOf(ctx, tileID)
	if err != nil {
		return t, m, err
	}
	if m.Engine != "postgres" {
		return t, m, errs.Invalidf("engine", "%s has no database metrics; only postgres does", m.Engine)
	}
	return t, m, nil
}

// PGMonitor reads the tab's model. The statements are read now, not stored.
func (o *Orchestrator) PGMonitor(ctx context.Context, tileID string) (PGMonitor, error) {
	t, m, err := o.pgInstance(ctx, tileID)
	if err != nil {
		return PGMonitor{}, err
	}
	var out PGMonitor
	o.metrics.mu.Lock()
	snap, ok := o.metrics.pg[tileID]
	o.metrics.mu.Unlock()
	if ok {
		out = pgMonitorOf(snap.last)
	}
	if st, ok := o.pitrState(ctx, m.ID); ok && st.ArchiverSeen && m.PITRDays > 0 {
		out.Archiving = true
	}
	cctx, cancel := context.WithTimeout(ctx, pgStatsLimit)
	defer cancel()
	dbs, err := o.pgOrgDatabases(ctx, t, m)
	if err == nil {
		out.Top, err = o.engines.PGTop(cctx, t, dbs)
	}
	if err != nil {
		out.TopError = "Statements are not available: the instance is stopped, or pg_stat_statements has no database yet."
	}
	return out, nil
}

// pgOrgDatabases are the instance's databases the instance's own org owns:
// the slices whose tile sits in that org. The instance serves slices of other
// orgs too, whose statements the viewer must not see.
func (o *Orchestrator) pgOrgDatabases(ctx context.Context, it Tile, m OperatedInstance) ([]string, error) {
	org, err := o.tileOrg(ctx, it)
	if err != nil {
		return nil, err
	}
	ps, err := o.operated.ByInstance(ctx, m.ID)
	if err != nil {
		return nil, err
	}
	var dbs []string
	for _, p := range ps {
		st, err := o.tiles.Get(ctx, p.TileID)
		if err != nil {
			continue
		}
		if so, err := o.tileOrg(ctx, st); err == nil && so == org {
			dbs = append(dbs, p.DBName)
		}
	}
	return dbs, nil
}

func pgMonitorOf(s mflow.PGSample) PGMonitor {
	out := PGMonitor{Sampled: true, At: s.At, ConnUsed: s.ConnUsed, ConnMax: s.ConnMax, OldestXactS: s.OldestXactS}
	for _, d := range s.DBs {
		out.DBs = append(out.DBs, pgLine(d.Name))
	}
	for _, r := range s.Replicas {
		out.Replicas = append(out.Replicas, pgLine(r.Name))
	}
	for _, sl := range s.Slots {
		out.Slots = append(out.Slots, pgLine(sl.Name))
	}
	for _, l := range [][]PGLine{out.DBs, out.Replicas, out.Slots} {
		sort.Slice(l, func(i, j int) bool { return l[i].Key < l[j].Key })
	}
	return out
}

// PGScope is the series the Database tab's open charts follow.
func (o *Orchestrator) PGScope(ctx context.Context, tileID string) (MetricsScope, error) {
	if _, _, err := o.pgInstance(ctx, tileID); err != nil {
		return MetricsScope{}, err
	}
	return MetricsScope{key: pgKey(tileID)}, nil
}

// PGMetrics is the instance's database series.
func (o *Orchestrator) PGMetrics(ctx context.Context, tileID string, in MetricsIn) (Series, error) {
	if _, _, err := o.pgInstance(ctx, tileID); err != nil {
		return nil, err
	}
	from, to, step := in.window(o.metricsKeep(ctx))
	return o.metricsStore().Query(ctx, pgKey(tileID), from, to, step)
}
