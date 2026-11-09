package env

import (
	"strings"
	"testing"

	"github.com/FyrmForge/stackr/internal/service"
	comp "github.com/FyrmForge/stackr/internal/ui/components"
	"github.com/FyrmForge/stackr/internal/ui/drawer/instance"
)

// The Database tab's model: a headline only once sampled, one chart line per
// database, standby and slot, the archive chart only while archiving, and
// the statements' figures in words.
func TestDatabaseView(t *testing.T) {
	d := databaseView(service.PGMonitor{}, comp.MetricsView{})
	if len(d.Headline) != 0 || d.Sampled {
		t.Errorf("unsampled has a headline: %+v", d)
	}
	for _, c := range d.Charts {
		if strings.HasPrefix(c.Measures, "db_") || c.Title == "WAL waiting to archive" {
			t.Errorf("chart for nothing: %+v", c)
		}
	}
	p := service.PGMonitor{Sampled: true, ConnUsed: 37, ConnMax: 100, OldestXactS: 90, Archiving: true,
		DBs: []string{"app", "postgres"}, Replicas: []string{"r1"}, Slots: []string{"s1"},
		Top: []service.PGQuery{{Calls: 7, TotalMS: 1500, MeanUS: 214000, Rows: 70, Text: "SELECT $1"}}}
	d = databaseView(p, comp.MetricsView{})
	if len(d.Headline) != 2 || d.Headline[0][1] != "37 of 100" || d.Headline[1][1] != "1m30s" {
		t.Errorf("headline = %v", d.Headline)
	}
	got := map[string]instance.ChartSpec{}
	for _, c := range d.Charts {
		got[c.Title] = c
	}
	if c := got["Database size"]; c.Measures != "db_app db_postgres" || c.Labels != "app,postgres" || c.Unit != "bytes" {
		t.Errorf("size chart = %+v", c)
	}
	for _, title := range []string{"Replica lag", "Replica lag (bytes)", "WAL held by slots", "WAL waiting to archive", "Connections"} {
		if _, ok := got[title]; !ok {
			t.Errorf("no %q chart", title)
		}
	}
	if len(d.Top) != 1 || d.Top[0].Total != "1.5s" || d.Top[0].Mean != "214ms" || d.Top[0].Calls != "7" {
		t.Errorf("top = %+v", d.Top)
	}
	if instance.Tab("database") != "database" {
		t.Error("database is not a tab")
	}
}
