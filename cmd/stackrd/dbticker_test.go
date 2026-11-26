package main

import (
	"testing"

	"github.com/FyrmForge/stackr/internal/paneldb"
)

func TestTickerRunsWheneverTheDatabaseIsOnADisk(t *testing.T) {
	for _, tc := range []struct {
		name string
		j    paneldb.Journal
		want bool
	}{
		{"unmoved", paneldb.Journal{Phase: paneldb.PhaseNone}, false},
		{"moved", paneldb.Journal{Phase: paneldb.PhaseMoved, Direction: paneldb.DirIn}, true},
		{"the new panel of a move-in", paneldb.Journal{Phase: paneldb.PhaseGating, Direction: paneldb.DirIn}, true},
		{"the new panel of an update on a moved install", paneldb.Journal{Phase: paneldb.PhaseGating, Direction: paneldb.DirUpdate, Moved: true}, true},
		{"the panel of a move-back", paneldb.Journal{Phase: paneldb.PhaseGating, Direction: paneldb.DirBack}, false},
		{"an ordinary update", paneldb.Journal{Phase: paneldb.PhaseGating, Direction: paneldb.DirUpdate}, false},
	} {
		dir := t.TempDir()
		if err := paneldb.WriteJournal(dir, tc.j); err != nil {
			t.Fatal(err)
		}
		if got := dbOnDisk(dir); got != tc.want {
			t.Errorf("%s: %v", tc.name, got)
		}
	}
}
