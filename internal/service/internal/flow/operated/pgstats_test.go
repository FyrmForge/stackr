package operated_test

import (
	"testing"
	"time"

	mflow "github.com/FyrmForge/stackr/internal/service/internal/flow/operated"
)

// The sampling statement's output parses into every metric; a name with a
// pipe in it stays whole because names come last.
func TestParsePGStats(t *testing.T) {
	out := "c|37|100|5|30|2\nx|900\nk|1024\n" +
		"d|1048576|100|4|900|100|1|app\nd|2048|0|0|0|0|0|we|ird\n" +
		"r|4096|12|replica1\ns|2048|slot_a\nzzz|ignored\n"
	s, err := mflow.ParsePGStats(out, time.Unix(1, 0))
	if err != nil {
		t.Fatal(err)
	}
	if s.ConnUsed != 37 || s.ConnMax != 100 || s.ConnActive != 5 || s.ConnIdle != 30 || s.ConnIdleTx != 2 {
		t.Errorf("connections = %+v", s)
	}
	if s.OldestXactS != 900 || s.SlotKeepMB != 1024 {
		t.Errorf("xact %d keep %d", s.OldestXactS, s.SlotKeepMB)
	}
	if len(s.DBs) != 2 || s.DBs[0].Name != "app" || s.DBs[0].Bytes != 1048576 || s.DBs[0].Commits != 100 ||
		s.DBs[0].BlksHit != 900 || s.DBs[0].Deadlk != 1 || s.DBs[1].Name != "we|ird" {
		t.Errorf("dbs = %+v", s.DBs)
	}
	if len(s.Replicas) != 1 || s.Replicas[0] != (mflow.PGReplica{Name: "replica1", Bytes: 4096, Seconds: 12}) {
		t.Errorf("replicas = %+v", s.Replicas)
	}
	if len(s.Slots) != 1 || s.Slots[0] != (mflow.PGSlot{Name: "slot_a", Bytes: 2048}) {
		t.Errorf("slots = %+v", s.Slots)
	}
	if _, err := mflow.ParsePGStats("c|x|y|1|2|3", time.Now()); err == nil {
		t.Error("a malformed connection line parsed")
	}
	if _, err := mflow.ParsePGStats("", time.Now()); err == nil {
		t.Error("an empty answer parsed")
	}
}

func TestParsePGTop(t *testing.T) {
	qs := mflow.ParsePGTop("10|2500|250000|40|SELECT * FROM t WHERE a = $1 | b\nnoise\n")
	if len(qs) != 1 || qs[0].Calls != 10 || qs[0].TotalMS != 2500 || qs[0].MeanUS != 250000 ||
		qs[0].Rows != 40 || qs[0].Text != "SELECT * FROM t WHERE a = $1 | b" {
		t.Errorf("top = %+v", qs)
	}
}
