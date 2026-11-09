package service

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"sync"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
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
)

var pgAlertKinds = []string{AlertPGConn, AlertPGLag, AlertPGSlot, AlertPGXact}

func pgKey(tileID string) string { return "pg:" + tileID }

var pgBadName = regexp.MustCompile(`[^A-Za-z0-9_]`)

// pgSeries makes a database, standby or slot name safe in a series name.
func pgSeries(name string) string { return pgBadName.ReplaceAllString(name, "_") }

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
	sum := func(s mflow.PGSample) (commit, rollback, hit, read, dead int64) {
		for _, d := range s.DBs {
			commit, rollback, hit, read, dead = commit+d.Commits, rollback+d.Rollbacks, hit+d.BlksHit, read+d.BlksRead, dead+d.Deadlk
		}
		return
	}
	pc, pr, ph, pd, pk := sum(*prev)
	cc, cr, ch, cd, ck := sum(cur)
	if cc < pc || cr < pr || ch < ph || cd < pd || ck < pk {
		return out
	}
	add("commits_s", float64(cc-pc)/dt)
	add("rollbacks_s", float64(cr-pr)/dt)
	add("deadlocks_s", float64(ck-pk)/dt)
	if reads := (ch - ph) + (cd - pd); reads > 0 {
		add("cache_hit", float64(ch-ph)/float64(reads))
	}
	return out
}

// pgProblem is one alert's verdict; Sev "" = healthy.
type pgProblem struct{ Sev, Msg string }

// pgAlerts judges a sample, one verdict per alert kind.
func pgAlerts(slug string, s mflow.PGSample) map[string]pgProblem {
	out := map[string]pgProblem{}
	if s.ConnMax > 0 && float64(s.ConnUsed) > pgConnWarn*float64(s.ConnMax) {
		out[AlertPGConn] = pgProblem{SevWarning, fmt.Sprintf("%s: %d of %d connections are in use.", slug, s.ConnUsed, s.ConnMax)}
	}
	for _, r := range s.Replicas {
		if time.Duration(r.Seconds)*time.Second > pgLagWarn {
			out[AlertPGLag] = pgProblem{SevWarning, fmt.Sprintf("%s: standby %s is %s behind.", slug, r.Name, agoWords(time.Duration(r.Seconds)*time.Second))}
			break
		}
	}
	if s.SlotKeepMB > 0 {
		cap := float64(s.SlotKeepMB) * (1 << 20)
		for _, sl := range s.Slots {
			frac := float64(sl.Bytes) / cap
			if frac < pgSlotWarn {
				continue
			}
			p := pgProblem{SevWarning, fmt.Sprintf("%s: replication slot %s holds %d%% of the WAL cap.", slug, sl.Name, int(frac*100))}
			if frac >= pgSlotCrit {
				p.Sev = SevCritical
			}
			if cur, ok := out[AlertPGSlot]; !ok || (cur.Sev == SevWarning && p.Sev == SevCritical) {
				out[AlertPGSlot] = p
			}
		}
	}
	if d := time.Duration(s.OldestXactS) * time.Second; d > pgXactWarn {
		out[AlertPGXact] = pgProblem{SevWarning, fmt.Sprintf("%s: a transaction has been open for %s.", slug, agoWords(d))}
	}
	return out
}

// pgSnap is what the panel remembers of an instance: its last sample.
type pgSnap struct{ last mflow.PGSample }

// pgTick samples every postgres instance that answers, writes the points and
// the alerts. A stopped instance or a failed read changes nothing.
func (o *Orchestrator) pgTick(ctx context.Context) []metrics.Point {
	ms, err := o.operated.WithEngine(ctx, "postgres")
	if err != nil {
		return nil
	}
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
			sem <- struct{}{}
			defer func() { <-sem }()
			cctx, cancel := context.WithTimeout(ctx, pgStatsLimit)
			defer cancel()
			t, err := o.tiles.Get(cctx, m.TileID)
			if err != nil {
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
			verdicts := pgAlerts(t.Slug, cur)
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
	DBs         []string // series suffixes, see pgSeries
	Replicas    []string
	Slots       []string
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
	if out.Top, err = o.engines.PGTop(cctx, t); err != nil {
		out.TopError = "Statements are not available: the instance is stopped, or pg_stat_statements has no database yet."
	}
	return out, nil
}

func pgMonitorOf(s mflow.PGSample) PGMonitor {
	out := PGMonitor{Sampled: true, At: s.At, ConnUsed: s.ConnUsed, ConnMax: s.ConnMax, OldestXactS: s.OldestXactS}
	for _, d := range s.DBs {
		out.DBs = append(out.DBs, pgSeries(d.Name))
	}
	for _, r := range s.Replicas {
		out.Replicas = append(out.Replicas, pgSeries(r.Name))
	}
	for _, sl := range s.Slots {
		out.Slots = append(out.Slots, pgSeries(sl.Name))
	}
	sort.Strings(out.DBs)
	sort.Strings(out.Replicas)
	sort.Strings(out.Slots)
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
