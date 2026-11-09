package service

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
	"github.com/FyrmForge/stackr/internal/service/internal/storetest"
	"github.com/google/uuid"
)

func pgSampleJSON(used string) string {
	return `{"conn":{"used":` + used + `,"max":100,"active":10,"idle":80,"idle_tx":5},"xact":0,"keep":null,` +
		`"dbs":[{"oid":1,"name":"app","bytes":4096,"commits":10,"rollbacks":1,"hit":90,"read":10,"deadlocks":0}],"replicas":[],"slots":[]}`
}

// A tick samples a running postgres instance, writes its series and opens
// the connection problem on the banner; once the load drops the problem
// clears; a stopped instance clears it too, a failed read does not.
func TestPGMonitorTick(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	o := w.orch
	pg, err := o.CreateOperatedTile(ctx, Tile{EnvironmentID: w.env, Name: "pg"}, "postgres")
	must(t, err)
	w.fake.Containers = []docker.Container{{
		ID: "c-pg", Name: "pg", State: "running",
		Labels: map[string]string{tile.LabelTile: pg.ID, tile.LabelRole: "replica"},
	}}
	var mu sync.Mutex
	used, fail := "95", false
	w.fake.ExecFn = func(cmd []string) string {
		mu.Lock()
		defer mu.Unlock()
		j := strings.Join(cmd, " ")
		if strings.Contains(j, "FROM pg_stat_activity") && !fail {
			if !strings.Contains(j, "statement_timeout") {
				t.Error("sampling session has no statement_timeout")
			}
			return pgSampleJSON(used)
		}
		return ""
	}
	connProblem := func() bool {
		ps, err := o.Problems(ctx)
		must(t, err)
		for _, p := range ps {
			if strings.HasPrefix(p.Key, "pgmon.conn.") {
				if !strings.Contains(p.Message, "95 of 100") || !strings.Contains(p.Link, "tab=database") {
					t.Errorf("problem = %+v", p)
				}
				return true
			}
		}
		return false
	}
	pts := o.pgTick(ctx)
	must(t, o.metricsStore().Write(ctx, pts))
	if !connProblem() {
		t.Fatal("no connections problem")
	}
	ser, err := o.PGMetrics(ctx, pg.ID, MetricsIn{})
	if err != nil || len(ser["conn_used"]) == 0 || len(ser["db_"+pgSeries("app")]) == 0 {
		t.Fatalf("series = %v, %v", ser, err)
	}
	mon, err := o.PGMonitor(ctx, pg.ID)
	if err != nil || !mon.Sampled || mon.ConnUsed != 95 || len(mon.DBs) != 1 || mon.DBs[0].Label != "app" {
		t.Fatalf("monitor = %+v, %v", mon, err)
	}

	mu.Lock()
	fail = true // a read that fails keeps the problem
	mu.Unlock()
	o.pgTick(ctx)
	if !connProblem() {
		t.Error("a failed read cleared the problem")
	}
	mu.Lock()
	fail = false
	mu.Unlock()

	w.fake.Containers[0].State = "exited" // conclusively stopped
	o.pgTick(ctx)
	if connProblem() {
		t.Error("problem stayed on a stopped instance")
	}

	w.fake.Containers[0].State = "running"
	o.pgTick(ctx)
	if !connProblem() {
		t.Fatal("problem did not return")
	}
	mu.Lock()
	used = "10"
	mu.Unlock()
	o.pgTick(ctx)
	if connProblem() {
		t.Error("problem stayed after recovery")
	}
}

// The statements the tab shows are those of slice databases the instance's
// own org owns: a slice another org cut from the instance is left out.
func TestPGTopOnlyOwnOrgDatabases(t *testing.T) {
	t.Parallel()
	w := newWorld(t)
	ctx := context.Background()
	o := w.orch
	pg, err := o.CreateOperatedTile(ctx, Tile{EnvironmentID: w.env, Name: "pg"}, "postgres")
	must(t, err)
	m, err := o.operated.GetByTile(ctx, pg.ID)
	must(t, err)

	now := time.Now()
	org2, stack2, env2 := uuid.NewString(), uuid.NewString(), uuid.NewString()
	must(t, w.st.Orgs.Create(ctx, store.Org{ID: org2, Name: "other", Slug: "other", EnvColors: "{}", Settings: "{}", CreatedAt: now}))
	must(t, w.st.Stacks.Create(ctx, store.Stack{ID: stack2, OrgID: org2, Name: "x", Slug: "x", Settings: "{}", CreatedAt: now}))
	must(t, w.st.Environments.Create(ctx, store.Environment{NodeID: storetest.Controller(t, w.st).ID, ID: env2, StackID: stack2,
		Name: "dev", Slug: "dev", Type: "static", Settings: "{}", Network: "n2", FromKind: "branch", FromBranch: "main", CreatedAt: now}))
	mine, err := o.CreateTile(ctx, Tile{StackID: w.stack, EnvironmentID: w.env, Name: "mine", Kind: tile.Image, ImageRef: "nginx:1", ContainerPort: 80})
	must(t, err)
	theirs, err := o.CreateTile(ctx, Tile{StackID: stack2, EnvironmentID: env2, Name: "theirs", Kind: tile.Image, ImageRef: "nginx:1", ContainerPort: 80})
	must(t, err)
	for tl, db := range map[string]string{mine.ID: "mine_db", theirs.ID: "their_db"} {
		must(t, w.st.Provisions.Create(ctx, store.Provision{ID: uuid.NewString(), TileID: tl, InstanceID: m.ID, DBName: db,
			DBUser: db, DBPassword: "x", CreatedAt: now}))
	}
	dbs, err := o.pgOrgDatabases(ctx, pg, m)
	must(t, err)
	if len(dbs) != 1 || dbs[0] != "mine_db" {
		t.Fatalf("databases = %v, want only mine_db", dbs)
	}

	w.fake.Containers = []docker.Container{{
		ID: "c-pg", Name: "pg", State: "running",
		Labels: map[string]string{tile.LabelTile: pg.ID, tile.LabelRole: "replica"},
	}}
	var asked string
	w.fake.ExecFn = func(cmd []string) string {
		if j := strings.Join(cmd, " "); strings.Contains(j, "pg_stat_statements") {
			asked = j
			return `[{"calls":7,"total_ms":1500,"mean_us":214000,"rows":70,"text":"SELECT $1"}]`
		}
		return ""
	}
	mon, err := o.PGMonitor(ctx, pg.ID)
	must(t, err)
	if len(mon.Top) != 1 || mon.Top[0].Calls != 7 {
		t.Fatalf("top = %+v (%s)", mon.Top, mon.TopError)
	}
	if !strings.Contains(asked, "'mine_db'") || strings.Contains(asked, "their_db") {
		t.Errorf("statements asked for the wrong databases: %s", asked)
	}
}
