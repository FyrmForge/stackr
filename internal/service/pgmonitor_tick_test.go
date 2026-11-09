package service

import (
	"context"
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service/internal/docker"
	"github.com/FyrmForge/stackr/internal/service/internal/leaf/tile"
)

// A tick samples a running postgres instance, writes its series and opens
// the connection problem on the banner; once the load drops the problem
// clears. The Database tab reads pg_stat_statements on demand.
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
	used := "95"
	w.fake.ExecFn = func(cmd []string) string {
		j := strings.Join(cmd, " ")
		switch {
		case strings.Contains(j, "FROM pg_stat_statements"):
			return "7|1500|214000|70|SELECT * FROM t WHERE a = $1\n"
		case strings.Contains(j, "FROM pg_stat_activity"):
			return "c|" + used + "|100|10|80|5\nx|0\nd|4096|10|1|90|10|0|app\n"
		}
		return ""
	}
	pts := o.pgTick(ctx)
	must(t, o.metricsStore().Write(ctx, pts))
	ps, err := o.Problems(ctx)
	must(t, err)
	found := false
	for _, p := range ps {
		if strings.HasPrefix(p.Key, "pgmon.conn.") && strings.Contains(p.Message, "95 of 100") && strings.Contains(p.Link, "tab=database") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no connections problem in %+v", ps)
	}
	ser, err := o.PGMetrics(ctx, pg.ID, MetricsIn{})
	if err != nil || len(ser["conn_used"]) == 0 || len(ser["db_app"]) == 0 {
		t.Fatalf("series = %v, %v", ser, err)
	}
	mon, err := o.PGMonitor(ctx, pg.ID)
	if err != nil || !mon.Sampled || mon.ConnUsed != 95 || len(mon.Top) != 1 || mon.Top[0].Calls != 7 ||
		len(mon.DBs) != 1 || mon.DBs[0] != "app" {
		t.Fatalf("monitor = %+v, %v", mon, err)
	}
	used = "10"
	o.pgTick(ctx)
	ps, _ = o.Problems(ctx)
	for _, p := range ps {
		if strings.HasPrefix(p.Key, "pgmon.") {
			t.Errorf("problem stayed after recovery: %+v", p)
		}
	}
}
