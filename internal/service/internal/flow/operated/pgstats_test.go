package operated_test

import (
	"testing"
	"time"

	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
)

// The sampling statement's JSON parses into every metric; names with a
// newline, a pipe or edge spaces stay whole.
func TestParsePGStats(t *testing.T) {
	out := `{"conn":{"used":37,"max":100,"active":5,"idle":30,"idle_tx":2},"xact":900,"keep":1024,` +
		`"dbs":[{"oid":16384,"name":"app","bytes":1048576,"commits":100,"rollbacks":4,"hit":900,"read":100,"deadlocks":1},` +
		`{"oid":5,"name":" we|ird\nname ","bytes":2048,"commits":0,"rollbacks":0,"hit":0,"read":0,"deadlocks":0}],` +
		`"replicas":[{"name":"replica1","bytes":4096,"seconds":12}],"slots":[{"name":"slot_a","bytes":2048}]}`
	s, err := mflow.ParsePGStats(out+"\n", time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if s.ConnUsed != 37 || s.ConnMax != 100 || s.ConnActive != 5 || s.ConnIdle != 30 || s.ConnIdleTx != 2 {
		t.Errorf("connections = %+v", s)
	}
	if s.OldestXactS != 900 || s.SlotKeepMB != 1024 {
		t.Errorf("xact %d keep %d", s.OldestXactS, s.SlotKeepMB)
	}
	if len(s.DBs) != 2 || s.DBs[0].Name != "app" || s.DBs[0].OID != 16384 || s.DBs[0].Bytes != 1048576 || s.DBs[0].Commits != 100 ||
		s.DBs[0].BlksHit != 900 || s.DBs[0].Deadlk != 1 || s.DBs[1].Name != " we|ird\nname " {
		t.Errorf("dbs = %+v", s.DBs)
	}
	if len(s.Replicas) != 1 || s.Replicas[0] != (mflow.PGReplica{Name: "replica1", Bytes: 4096, Seconds: 12}) {
		t.Errorf("replicas = %+v", s.Replicas)
	}
	if len(s.Slots) != 1 || s.Slots[0] != (mflow.PGSlot{Name: "slot_a", Bytes: 2048}) {
		t.Errorf("slots = %+v", s.Slots)
	}
	for _, bad := range []string{"", "c|1|2", `{"conn":{"used":1}}`} {
		if _, err := mflow.ParsePGStats(bad, time.Now()); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestParsePGTop(t *testing.T) {
	qs := mflow.ParsePGTop(`[{"calls":10,"total_ms":2500,"mean_us":250000,"rows":40,"text":"SELECT *\n  FROM t WHERE a = $1 | b"}]`)
	if len(qs) != 1 || qs[0].Calls != 10 || qs[0].TotalMS != 2500 || qs[0].MeanUS != 250000 ||
		qs[0].Rows != 40 || qs[0].Text != "SELECT * FROM t WHERE a = $1 | b" {
		t.Errorf("top = %+v", qs)
	}
	if mflow.ParsePGTop("noise") != nil {
		t.Error("noise parsed")
	}
}
