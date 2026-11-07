package backup

import (
	"slices"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

func seg(n int) string {
	return "0000000100000000000000" + string("0123456789ABCDEF"[n/16]) + string("0123456789ABCDEF"[n%16])
}

func base(id string, finished time.Time, wal string) store.BackupRun {
	return store.BackupRun{ID: id, Status: Done, ObjectKey: "k/" + id, FinishedAt: &finished, WalStart: &wal}
}

func TestWindow(t *testing.T) {
	now := time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)
	day := func(n int) time.Time { return now.AddDate(0, 0, -n) }
	failed := base("failed", day(0), seg(90))
	failed.Status, failed.ObjectKey = Failed, ""
	noWal := base("nowal", day(30), "")
	noWal.WalStart = nil
	for _, c := range []struct {
		name     string
		runs     []store.BackupRun
		days     int
		wantDrop []string
		wantWal  string
		wantOK   bool
	}{
		{name: "no runs", days: 7},
		{name: "only failed and unrecorded", runs: []store.BackupRun{failed, noWal}, days: 7},
		{
			name:    "one inside the window is kept",
			runs:    []store.BackupRun{base("a", day(1), seg(10))},
			days:    7,
			wantWal: seg(10), wantOK: true,
		},
		{
			name:    "one older than the window is the anchor and stays",
			runs:    []store.BackupRun{base("a", day(20), seg(10))},
			days:    7,
			wantWal: seg(10), wantOK: true,
		},
		{
			name: "window keeps the newer ones and one older anchor, drops the rest",
			runs: []store.BackupRun{
				base("w1", day(1), seg(50)), base("w2", day(6), seg(40)),
				base("anchor", day(9), seg(30)), base("old1", day(16), seg(20)), base("old2", day(23), seg(10)),
			},
			days:     7,
			wantDrop: []string{"old1", "old2"},
			wantWal:  seg(30), wantOK: true,
		},
		{
			name: "order of the input does not matter",
			runs: []store.BackupRun{
				base("old1", day(16), seg(20)), base("w1", day(1), seg(50)), base("anchor", day(9), seg(30)),
			},
			days:     7,
			wantDrop: []string{"old1"},
			wantWal:  seg(30), wantOK: true,
		},
		{
			name:    "a failed run changes nothing",
			runs:    []store.BackupRun{failed, base("a", day(2), seg(10)), base("b", day(10), seg(5))},
			days:    7,
			wantWal: seg(5), wantOK: true,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			drop, wal, ok := Window(c.runs, now, c.days)
			var ids []string
			for _, r := range drop {
				ids = append(ids, r.ID)
			}
			slices.Sort(ids)
			if !slices.Equal(ids, c.wantDrop) || wal != c.wantWal || ok != c.wantOK {
				t.Errorf("drop %v wal %s ok %v, want %v %s %v", ids, wal, ok, c.wantDrop, c.wantWal, c.wantOK)
			}
		})
	}
}

func TestStaleWAL(t *testing.T) {
	root := "stackr/o/v/pitr"
	keys := []string{
		WALKey(root, seg(1), seg(4)),
		WALKey(root, seg(5), seg(9)),
		WALKey(root, seg(10), seg(14)), // ends after the cut: kept
		root + "/wal/00000002.history-00000002.history" + BundleExt,
		root + "/wal/not-a-bundle.txt",
		root + "/base/20261107-120000-pg.tar.gz.age",
	}
	got := StaleWAL(keys, seg(10))
	if want := keys[:2]; !slices.Equal(got, want) {
		t.Errorf("stale = %v, want %v", got, want)
	}
	// a bundle that ends at the cut itself holds the base's first segment
	if got := StaleWAL([]string{WALKey(root, seg(5), seg(10))}, seg(10)); len(got) != 0 {
		t.Errorf("a bundle ending at wal_start was dropped: %v", got)
	}
}

func TestWALNames(t *testing.T) {
	for name, want := range map[string]bool{
		seg(3):                       true,
		seg(3) + ".00000028.backup":  true,
		"00000002.history":           true,
		seg(3) + ".partial":          false,
		seg(3) + ".tmp":              false,
		"." + seg(3):                 false,
		"00000002.hist":              false,
		"0000000100000000000000G1":   false,
		"x; rm -rf /":                false,
		"":                           false,
		seg(3)[:23]:                  false,
		"00000001000000000000000100": false,
		"0000000100000000000000aa":   false,
	} {
		if WALName(name) != want {
			t.Errorf("WALName(%q) = %v, want %v", name, !want, want)
		}
	}
}

func TestBundleLast(t *testing.T) {
	last, hist, ok := BundleLast(WALKey("r", seg(1), seg(4)))
	if last != seg(4) || hist || !ok {
		t.Errorf("segment bundle: %q %v %v", last, hist, ok)
	}
	if _, hist, ok := BundleLast("r/wal/00000002.history-00000002.history" + BundleExt); !hist || !ok {
		t.Errorf("history bundle: %v %v", hist, ok)
	}
	if _, _, ok := BundleLast("r/wal/junk" + BundleExt); ok {
		t.Error("junk read as a bundle")
	}
}

func TestIsPITRKey(t *testing.T) {
	if !IsPITRKey(PITRRoot("o", "v")+"/base/x.tar.gz.age") || IsPITRKey(Prefix("o", "v", "s1")+"/x.sql.gz.age") {
		t.Error("IsPITRKey")
	}
}
