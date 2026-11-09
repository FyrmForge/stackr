package operated

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

// PGStatsSQL is the whole sample in one statement, as one JSON document, so
// a name with a newline or a pipe in it stays whole. A standby has no current
// WAL position, hence the CASE.
const PGStatsSQL = `WITH w AS (SELECT CASE WHEN pg_is_in_recovery() THEN pg_last_wal_replay_lsn() ELSE pg_current_wal_lsn() END AS lsn)
SELECT json_build_object(
 'conn', (SELECT json_build_object('used', count(*), 'max', (SELECT setting::int FROM pg_settings WHERE name = 'max_connections'),
    'active', count(*) FILTER (WHERE state = 'active'), 'idle', count(*) FILTER (WHERE state = 'idle'),
    'idle_tx', count(*) FILTER (WHERE state LIKE 'idle in transaction%'))
    FROM pg_stat_activity WHERE backend_type = 'client backend'),
 'xact', (SELECT coalesce(max(extract(epoch FROM now() - xact_start)), 0)::bigint FROM pg_stat_activity
    WHERE backend_type = 'client backend' AND xact_start IS NOT NULL AND pid <> pg_backend_pid()),
 'keep', (SELECT setting::bigint FROM pg_settings WHERE name = 'max_slot_wal_keep_size'),
 'dbs', (SELECT coalesce(json_agg(json_build_object('oid', d.oid::bigint, 'name', d.datname, 'bytes', pg_database_size(d.datname),
    'commits', s.xact_commit, 'rollbacks', s.xact_rollback, 'hit', s.blks_hit, 'read', s.blks_read, 'deadlocks', s.deadlocks)), '[]'::json)
    FROM pg_database d JOIN pg_stat_database s ON s.datid = d.oid WHERE d.datallowconn AND NOT d.datistemplate),
 'replicas', (SELECT coalesce(json_agg(json_build_object('name', coalesce(application_name, ''),
    'bytes', coalesce(pg_wal_lsn_diff((SELECT lsn FROM w), replay_lsn), 0)::bigint,
    'seconds', coalesce(extract(epoch FROM replay_lag), 0)::bigint)), '[]'::json) FROM pg_stat_replication),
 'slots', (SELECT coalesce(json_agg(json_build_object('name', slot_name,
    'bytes', coalesce(pg_wal_lsn_diff((SELECT lsn FROM w), restart_lsn), 0)::bigint)), '[]'::json) FROM pg_replication_slots))`

// PGStatsTopSQL is the ten statements that took the most time in the
// databases named in %s (a SQL list). Text is cut; pg_stat_statements
// already holds $1, not the literals.
const PGStatsTopSQL = `SELECT coalesce(json_agg(t), '[]'::json) FROM (
 SELECT calls, round(total_exec_time)::bigint AS total_ms, round(mean_exec_time * 1000)::bigint AS mean_us, rows, left(query, 300) AS text
 FROM pg_stat_statements WHERE dbid IN (SELECT oid FROM pg_database WHERE datname IN (%s))
 ORDER BY total_exec_time DESC LIMIT 10) t`

// pgStatementTimeout bounds each sampling statement in the server, so a wedged
// query frees its session even when the panel gave up on it.
const pgStatementTimeout = `SET statement_timeout = '5s'`

// PGDatabase is one database's size and its cumulative counters.
type PGDatabase struct {
	OID                       int64
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
	Calls   int64  `json:"calls"`
	TotalMS int64  `json:"total_ms"`
	MeanUS  int64  `json:"mean_us"`
	Rows    int64  `json:"rows"`
	Text    string `json:"text"`
}

// ParsePGStats reads PGStatsSQL's output.
func ParsePGStats(out string, at time.Time) (PGSample, error) {
	var j struct {
		Conn struct {
			Used, Max, Active, Idle int
			IdleTx                  int `json:"idle_tx"`
		}
		Xact int64
		Keep *int64
		DBs  []struct {
			OID       int64 `json:"oid"`
			Name      string
			Bytes     int64
			Commits   int64
			Rollbacks int64
			Hit, Read int64
			Deadlocks int64
		}
		Replicas []PGReplica
		Slots    []PGSlot
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &j); err != nil {
		return PGSample{}, errs.Invalidf("stats", "unreadable sample")
	}
	if j.Conn.Max == 0 {
		return PGSample{}, errs.Invalidf("stats", "no connection data")
	}
	s := PGSample{At: at, ConnUsed: j.Conn.Used, ConnMax: j.Conn.Max, ConnActive: j.Conn.Active, ConnIdle: j.Conn.Idle,
		ConnIdleTx: j.Conn.IdleTx, OldestXactS: j.Xact, Replicas: j.Replicas, Slots: j.Slots}
	if j.Keep != nil {
		s.SlotKeepMB = *j.Keep
	}
	for _, d := range j.DBs {
		s.DBs = append(s.DBs, PGDatabase{OID: d.OID, Name: d.Name, Bytes: d.Bytes, Commits: d.Commits, Rollbacks: d.Rollbacks,
			BlksHit: d.Hit, BlksRead: d.Read, Deadlk: d.Deadlocks})
	}
	return s, nil
}

// ParsePGTop reads PGStatsTopSQL's output; text that is not it yields none.
func ParsePGTop(out string) []PGQuery {
	var qs []PGQuery
	if json.Unmarshal([]byte(strings.TrimSpace(out)), &qs) != nil {
		return nil
	}
	for i := range qs {
		qs[i].Text = strings.Join(strings.Fields(qs[i].Text), " ")
	}
	return qs
}

// PGStats samples a running postgres instance with one psql exec.
func (f *Flow) PGStats(ctx context.Context, it store.Tile) (PGSample, error) {
	_, i, x, err := f.postgres(ctx, it)
	if err != nil {
		return PGSample{}, err
	}
	out, err := x.Exec(ctx, append(pg{}.psql(i, i.AdminDB, nil), "-tA", "-c", pgStatementTimeout, "-c", PGStatsSQL))
	if err != nil {
		return PGSample{}, err
	}
	return ParsePGStats(out, time.Now())
}

// PGTop is the instance's most expensive statements among dbs only:
// pg_stat_statements lists every database's, and slices of one instance may
// belong to other orgs. It is created in each slice's database, so the first
// of dbs is where to ask; none = none to show.
func (f *Flow) PGTop(ctx context.Context, it store.Tile, dbs []string) ([]PGQuery, error) {
	if len(dbs) == 0 {
		return nil, nil
	}
	_, i, x, err := f.postgres(ctx, it)
	if err != nil {
		return nil, err
	}
	q := make([]string, len(dbs))
	for n, d := range dbs {
		q[n] = "'" + strings.ReplaceAll(d, "'", "''") + "'"
	}
	out, err := x.Exec(ctx, append(pg{}.psql(i, dbs[0], nil), "-tA", "-c", pgStatementTimeout, "-c",
		fmt.Sprintf(PGStatsTopSQL, strings.Join(q, ","))))
	if err != nil {
		return nil, err
	}
	return ParsePGTop(out), nil
}
