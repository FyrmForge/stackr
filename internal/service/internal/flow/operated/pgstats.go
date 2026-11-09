package operated

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// PGStatsSQL is the whole sample in one statement: one line per fact, the
// kind first and any free text (a name) last, so a "|" in it cannot shift
// the numbers. A standby has no current WAL position, hence the CASE.
const PGStatsSQL = `WITH w AS (SELECT CASE WHEN pg_is_in_recovery() THEN pg_last_wal_replay_lsn() ELSE pg_current_wal_lsn() END AS lsn)
SELECT 'c|' || count(*) || '|' || (SELECT setting FROM pg_settings WHERE name = 'max_connections') ||
  '|' || count(*) FILTER (WHERE state = 'active') || '|' || count(*) FILTER (WHERE state = 'idle') ||
  '|' || count(*) FILTER (WHERE state LIKE 'idle in transaction%')
  FROM pg_stat_activity WHERE backend_type = 'client backend'
UNION ALL SELECT 'x|' || coalesce(max(extract(epoch FROM now() - xact_start)), 0)::bigint
  FROM pg_stat_activity WHERE backend_type = 'client backend' AND xact_start IS NOT NULL AND pid <> pg_backend_pid()
UNION ALL SELECT 'k|' || setting FROM pg_settings WHERE name = 'max_slot_wal_keep_size'
UNION ALL SELECT 'd|' || pg_database_size(d.datname) || '|' || s.xact_commit || '|' || s.xact_rollback ||
  '|' || s.blks_hit || '|' || s.blks_read || '|' || s.deadlocks || '|' || d.datname
  FROM pg_database d JOIN pg_stat_database s ON s.datid = d.oid WHERE d.datallowconn AND NOT d.datistemplate
UNION ALL SELECT 'r|' || coalesce(pg_wal_lsn_diff((SELECT lsn FROM w), replay_lsn), 0)::bigint ||
  '|' || coalesce(extract(epoch FROM replay_lag), 0)::bigint || '|' || coalesce(application_name, '')
  FROM pg_stat_replication
UNION ALL SELECT 's|' || coalesce(pg_wal_lsn_diff((SELECT lsn FROM w), restart_lsn), 0)::bigint || '|' || slot_name
  FROM pg_replication_slots`

// PGStatsTopSQL is the ten statements that took the most time. Text is cut,
// flattened and last on the line; pg_stat_statements already holds $1, not
// the literals.
const PGStatsTopSQL = `SELECT calls || '|' || round(total_exec_time)::bigint || '|' || round(mean_exec_time * 1000)::bigint ||
  '|' || rows || '|' || replace(replace(left(query, 300), E'\n', ' '), E'\r', ' ')
  FROM pg_stat_statements ORDER BY total_exec_time DESC LIMIT 10`

// PGDatabase is one database's size and its cumulative counters.
type PGDatabase struct {
	Name                      string
	Bytes                     int64
	Commits, Rollbacks        int64
	BlksHit, BlksRead, Deadlk int64
}

// PGReplica is one standby's lag behind the primary.
type PGReplica struct {
	Name    string
	Bytes   int64
	Seconds int64
}

// PGSlot is the WAL a replication slot holds back.
type PGSlot struct {
	Name  string
	Bytes int64
}

// PGSample is one read of an instance.
type PGSample struct {
	At                   time.Time
	ConnUsed, ConnMax    int
	ConnActive, ConnIdle int
	ConnIdleTx           int
	OldestXactS          int64
	SlotKeepMB           int64 // max_slot_wal_keep_size; -1 or 0 = no cap
	DBs                  []PGDatabase
	Replicas             []PGReplica
	Slots                []PGSlot
}

// PGQuery is one row of pg_stat_statements.
type PGQuery struct {
	Calls, TotalMS, MeanUS, Rows int64
	Text                         string
}

func ints(s string, n int) ([]int64, bool) {
	p := strings.Split(s, "|")
	if len(p) < n {
		return nil, false
	}
	out := make([]int64, n)
	for i := range n {
		v, err := strconv.ParseInt(p[i], 10, 64)
		if err != nil {
			return nil, false
		}
		out[i] = v
	}
	return out, true
}

// ParsePGStats reads PGStatsSQL's output; a line it does not know is
// skipped, a known one that is malformed fails the sample.
func ParsePGStats(out string, at time.Time) (PGSample, error) {
	s := PGSample{At: at}
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		kind, rest, ok := strings.Cut(strings.TrimSpace(l), "|")
		if !ok {
			continue
		}
		bad := errs.Invalidf("stats", "unreadable %s line", kind)
		switch kind {
		case "c":
			v, ok := ints(rest, 5)
			if !ok {
				return s, bad
			}
			s.ConnUsed, s.ConnMax, s.ConnActive, s.ConnIdle, s.ConnIdleTx = int(v[0]), int(v[1]), int(v[2]), int(v[3]), int(v[4])
		case "x":
			v, ok := ints(rest, 1)
			if !ok {
				return s, bad
			}
			s.OldestXactS = v[0]
		case "k":
			v, ok := ints(rest, 1)
			if !ok {
				return s, bad
			}
			s.SlotKeepMB = v[0]
		case "d":
			v, ok := ints(rest, 6)
			if !ok {
				return s, bad
			}
			s.DBs = append(s.DBs, PGDatabase{Name: nameOf(rest, 6), Bytes: v[0], Commits: v[1], Rollbacks: v[2],
				BlksHit: v[3], BlksRead: v[4], Deadlk: v[5]})
		case "r":
			v, ok := ints(rest, 2)
			if !ok {
				return s, bad
			}
			s.Replicas = append(s.Replicas, PGReplica{Name: nameOf(rest, 2), Bytes: v[0], Seconds: v[1]})
		case "s":
			v, ok := ints(rest, 1)
			if !ok {
				return s, bad
			}
			s.Slots = append(s.Slots, PGSlot{Name: nameOf(rest, 1), Bytes: v[0]})
		}
	}
	if s.ConnMax == 0 {
		return s, errs.Invalidf("stats", "no connection line")
	}
	return s, nil
}

// nameOf is what follows the first n "|"-separated fields.
func nameOf(s string, n int) string {
	p := strings.SplitN(s, "|", n+1)
	if len(p) <= n {
		return ""
	}
	return p[n]
}

// ParsePGTop reads PGStatsTopSQL's output.
func ParsePGTop(out string) []PGQuery {
	var qs []PGQuery
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		v, ok := ints(l, 4)
		if !ok {
			continue
		}
		qs = append(qs, PGQuery{Calls: v[0], TotalMS: v[1], MeanUS: v[2], Rows: v[3], Text: strings.TrimSpace(nameOf(l, 4))})
	}
	return qs
}

// PGStats samples a running postgres instance with one psql exec.
func (f *Flow) PGStats(ctx context.Context, it store.Tile) (PGSample, error) {
	_, i, x, err := f.postgres(ctx, it)
	if err != nil {
		return PGSample{}, err
	}
	out, err := x.Exec(ctx, append(pg{}.psql(i, i.AdminDB, nil), "-tA", "-c", PGStatsSQL))
	if err != nil {
		return PGSample{}, err
	}
	return ParsePGStats(out, time.Now())
}

// PGTop is the instance's most expensive statements. pg_stat_statements is
// created in each slice's database, not the admin one, and lists every
// database's statements, so any slice's will do.
func (f *Flow) PGTop(ctx context.Context, it store.Tile) ([]PGQuery, error) {
	m, i, x, err := f.postgres(ctx, it)
	if err != nil {
		return nil, err
	}
	db := i.AdminDB
	if ps, err := f.Instances.ByInstance(ctx, m.ID); err == nil && len(ps) > 0 {
		db = ps[0].DBName
	}
	out, err := x.Exec(ctx, append(pg{}.psql(i, db, nil), "-tA", "-c", PGStatsTopSQL))
	if err != nil {
		return nil, err
	}
	return ParsePGTop(out), nil
}
