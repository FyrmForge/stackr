package backup

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/FyrmForge/stackr/internal/service/errs"
	"github.com/FyrmForge/stackr/internal/service/internal/store"
)

const (
	rs1 = "000000010000000000000001"
	rs2 = "000000010000000000000002"
	rs3 = "000000010000000000000003"
	rs4 = "000000010000000000000004"
	rt2 = "000000020000000000000004" // timeline 2
)

var t0 = time.Date(2026, 11, 7, 12, 0, 0, 0, time.UTC)

// pbase is a finished base run that finished at ago before t0, with its key.
func pbase(ago time.Duration, start, stop string) store.BackupRun {
	f := t0.Add(-ago)
	r := store.BackupRun{
		ID: "r-" + f.Format("150405"), Status: Done, FinishedAt: &f,
		ObjectKey: "stackr/o/v/pitr/base/" + f.Format("20060102-150405") + "-" + start + "-data.tar.gz.age",
		WalStop:   &stop,
	}
	if start != "" {
		r.WalStart = &start
	}
	return r
}

func wal(first, last string) string { return "stackr/o/v/pitr/wal/" + first + "-" + last + BundleExt }

const hist = "stackr/o/v/pitr/wal/00000002.history-00000002.history" + BundleExt

func TestBundleFirstLastAndBaseWalStart(t *testing.T) {
	t.Parallel()
	k := wal(rs1, rs3)
	if f, h, ok := BundleFirst(k); f != rs1 || h || !ok {
		t.Errorf("BundleFirst = %q %v %v", f, h, ok)
	}
	if l, h, ok := BundleLast(k); l != rs3 || h || !ok {
		t.Errorf("BundleLast = %q %v %v", l, h, ok)
	}
	if _, h, ok := BundleFirst(hist); !h || !ok {
		t.Errorf("history = %v %v", h, ok)
	}
	if _, _, ok := BundleFirst("stackr/o/v/pitr/wal/junk.tar.gz.age"); ok {
		t.Error("junk read as a bundle")
	}
	for key, want := range map[string]string{
		pbase(0, rs2, rs3).ObjectKey:                   rs2,
		"x/base/20261107-120000-data.tar.gz.age":       "", // no start in the name
		"x/base/20261107-120000-" + rs2 + "-d.tar.age": rs2,
		"x/base/20261107-120000-" + rs2 + "d":          "", // not followed by -slug
		"x/base/short":                                 "",
	} {
		if got := BaseWalStart(key); got != want {
			t.Errorf("BaseWalStart(%q) = %q, want %q", key, got, want)
		}
	}
}

func TestPickBase(t *testing.T) {
	t.Parallel()
	h := time.Hour
	newest := pbase(1*h, rs3, rs3)
	older := pbase(30*h, rs1, rs2)
	oldest := pbase(100*h, rs1, rs1)
	failed := pbase(2*h, rs3, rs3)
	failed.Status = Failed
	unstopped := pbase(3*h, rs3, "") // a base whose end was never read
	unstopped.WalStop = nil
	keys := []string{wal(rs1, rs2), wal(rs3, rs4), hist}

	cases := []struct {
		name    string
		bases   []store.BackupRun
		keys    []string
		at      time.Time
		latest  bool
		want    string // the picked base's object key, "" = refused
		errKind string // conflict | invalid
		bundles int
	}{
		{name: "newest before T", bases: []store.BackupRun{older, newest, oldest}, keys: keys, at: t0, want: newest.ObjectKey, bundles: 2}, // hist + (rs3-rs4)
		{name: "T between two bases picks the earlier", bases: []store.BackupRun{older, newest}, keys: keys, at: t0.Add(-10 * h), want: older.ObjectKey, bundles: 3},
		{name: "latest takes the newest", bases: []store.BackupRun{older, newest}, keys: keys, latest: true, want: newest.ObjectKey, bundles: 2},
		{name: "a base finished after T is not used", bases: []store.BackupRun{newest}, keys: keys, at: t0.Add(-2 * h), errKind: "invalid"},
		{name: "failed and unfinished bases never count", bases: []store.BackupRun{failed, unstopped}, keys: keys, at: t0, errKind: "conflict"},
		{name: "wal_stop not covered by shipped WAL", bases: []store.BackupRun{pbase(h, rs1, rs4)}, keys: []string{wal(rs1, rs3)}, at: t0, errKind: "conflict"},
		{name: "wal_stop on another timeline is not covered", bases: []store.BackupRun{pbase(h, rs1, rt2)}, keys: []string{wal(rs1, rs4)}, at: t0, errKind: "conflict"},
		{name: "no WAL at all", bases: []store.BackupRun{newest}, keys: nil, at: t0, errKind: "conflict"},
		{name: "no bases", keys: keys, at: t0, errKind: "conflict"},
		{name: "an uncovered newest falls back to a covered older one", bases: []store.BackupRun{pbase(h, rs1, rs4), older}, keys: []string{wal(rs1, rs3)}, at: t0, want: older.ObjectKey, bundles: 1},
		{name: "no start in the row or the key", bases: []store.BackupRun{pbase(h, "", rs3)}, keys: keys, at: t0, errKind: "conflict"},
	}
	// a base whose row lost its start but whose key names it still plans
	rowless := pbase(h, rs3, rs3)
	rowless.WalStart = nil
	cases = append(cases, struct {
		name    string
		bases   []store.BackupRun
		keys    []string
		at      time.Time
		latest  bool
		want    string
		errKind string
		bundles int
	}{name: "start parsed from the key", bases: []store.BackupRun{rowless}, keys: keys, at: t0, want: rowless.ObjectKey, bundles: 2})

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			p, err := PickBase(c.bases, c.keys, Lineage{}, c.at, c.latest)
			if c.want == "" {
				switch c.errKind {
				case "conflict":
					if _, ok := errs.IsConflict(err); !ok {
						t.Fatalf("err = %v, want a conflict", err)
					}
				case "invalid":
					if _, ok := errs.IsInvalid(err); !ok {
						t.Fatalf("err = %v, want invalid", err)
					}
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if p.Base.ObjectKey != c.want || len(p.Bundles) != c.bundles {
				t.Errorf("base %s with %d bundles, want %s with %d", p.Base.ObjectKey, len(p.Bundles), c.want, c.bundles)
			}
		})
	}
}

// The bundles a plan reads: history always, segments from the base's start
// on, and the count of segments for the disk check.
func TestPickBaseBundlesAndSegments(t *testing.T) {
	t.Parallel()
	b := pbase(time.Hour, rs2, rs3)
	keys := []string{wal(rs1, rs1), wal(rs2, rs3), wal(rs4, rs4), hist, "stackr/o/v/pitr/wal/not-a-bundle"}
	p, err := PickBase([]store.BackupRun{b}, keys, Lineage{}, t0, false)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, x := range p.Bundles {
		got = append(got, x.Key)
	}
	if want := []string{hist, wal(rs2, rs3), wal(rs4, rs4)}; !slices.Equal(got, want) {
		t.Errorf("bundles = %v, want %v", got, want)
	}
	if p.Segments != 3 || p.WalStart != rs2 {
		t.Errorf("segments %d start %s, want 3 and %s", p.Segments, p.WalStart, rs2)
	}
	// 256 segments of a log file roll over into the next log id
	if n := segIndex("0000000100000001" + "00000000"); n != 256 {
		t.Errorf("segIndex = %d", n)
	}
}

func TestRestorableIsTheOldestUsableBase(t *testing.T) {
	t.Parallel()
	h := time.Hour
	keys := []string{wal(rs1, rs4)}
	from, ok := Restorable([]store.BackupRun{pbase(2*h, rs1, rs2), pbase(50*h, rs1, rs1), pbase(1*h, rs1, "000000010000000000000009")}, keys, Lineage{})
	if !ok || !from.Equal(t0.Add(-50*h)) {
		t.Errorf("from = %v %v", from, ok)
	}
	if _, ok := Restorable(nil, keys, Lineage{}); ok {
		t.Error("restorable with no base")
	}
}

// A bundle shipped across a promote (older shippers wrote them) still serves
// a base that stops on the new timeline.
func TestUsableReadsABundleSpanningTimelines(t *testing.T) {
	t.Parallel()
	keys := []string{wal(rs1, rt2)}
	if _, ok := Restorable([]store.BackupRun{pbase(time.Hour, rs1, rt2)}, keys, Lineage{}); !ok {
		t.Error("base on timeline 2 refused for a spanning bundle")
	}
	if _, ok := Restorable([]store.BackupRun{pbase(time.Hour, rs1, rs3)}, keys, Lineage{}); !ok {
		t.Error("base on timeline 1 refused for a spanning bundle")
	}
	if _, ok := Restorable([]store.BackupRun{pbase(time.Hour, rs1, rt2)}, []string{wal(rs1, rs4)}, Lineage{}); ok {
		t.Error("base on timeline 2 accepted with only timeline 1 shipped")
	}
}

func TestParseAt(t *testing.T) {
	t.Parallel()
	if _, latest, err := ParseAt("latest"); err != nil || !latest {
		t.Errorf("latest: %v %v", latest, err)
	}
	at, latest, err := ParseAt("2026-11-07T11:42:00+01:00")
	if err != nil || latest || !at.Equal(time.Date(2026, 11, 7, 10, 42, 0, 0, time.UTC)) || at.Location() != time.UTC {
		t.Errorf("offset: %v %v %v", at, latest, err)
	}
	if _, _, err := ParseAt("2026-11-07T10:42:00Z"); err != nil {
		t.Errorf("Z: %v", err)
	}
	for _, bad := range []string{"", "yesterday", "2026-11-07", "2026-11-07 10:42", "2026-11-07T10:42:00", "Latest"} {
		if _, _, err := ParseAt(bad); err == nil {
			t.Errorf("ParseAt(%q) took it", bad)
		} else if _, ok := errs.IsInvalid(err); !ok {
			t.Errorf("ParseAt(%q) = %v, want invalid", bad, err)
		}
	}
}

func TestCheckAtBoundsAndBranchMargin(t *testing.T) {
	t.Parallel()
	from, to := t0.Add(-48*time.Hour), t0.Add(-time.Minute)
	for name, c := range map[string]struct {
		at     time.Time
		latest bool
		branch bool
		now    time.Time
		bad    bool
	}{
		"inside":                   {at: t0.Add(-time.Hour), now: t0},
		"on the lower bound":       {at: from, now: t0},
		"before the lower bound":   {at: from.Add(-time.Second), now: t0, bad: true},
		"after the upper bound":    {at: to.Add(time.Second), now: t0, bad: true},
		"latest needs no bound":    {latest: true, now: t0},
		"branch an hour ago":       {at: t0.Add(-time.Hour), branch: true, now: t0},
		"branch within 2 minutes":  {at: t0.Add(-90 * time.Second), branch: true, now: t0, bad: true},
		"branch just over 2 min":   {at: t0.Add(-130 * time.Second), branch: true, now: t0},
		"restore within 2 minutes": {at: t0.Add(-90 * time.Second), now: t0},
		"branch latest":            {latest: true, branch: true, now: t0},
	} {
		err := CheckAt(c.at, c.latest, from, to, c.now, c.branch)
		if (err != nil) != c.bad {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	// both bounds are in the message
	err := CheckAt(t0.Add(-100*time.Hour), false, from, to, t0, false)
	if err == nil || !strings.Contains(err.Error(), from.Format(time.RFC3339)) || !strings.Contains(err.Error(), to.Format(time.RFC3339)) {
		t.Errorf("error does not name both bounds: %v", err)
	}
	err = CheckAt(t0.Add(-90*time.Second), false, from, t0, t0, true)
	if err == nil || !strings.Contains(err.Error(), "branch without --at") {
		t.Errorf("margin error = %v", err)
	}
}

func TestDiskNeed(t *testing.T) {
	t.Parallel()
	if got, want := DiskNeed(10<<30, 4), int64(10<<30+4*SegmentBytes)*12/10; got != want {
		t.Errorf("DiskNeed = %d, want %d", got, want)
	}
}

// The base's stop segment must sit inside a shipped bundle; some later bundle
// existing proves nothing (the one holding the stop may be gone).
func TestUsableNeedsTheStopInsideABundle(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		stop string
		keys []string
		ok   bool
	}{
		"inside":                {rs2, []string{wal(rs1, rs3)}, true},
		"at the bundle's edges": {rs3, []string{wal(rs1, rs1), wal(rs3, rs4)}, true},
		"its bundle is gone":    {rs2, []string{wal(rs3, rs4)}, false},
		"a gap before a later":  {rs2, []string{wal(rs1, rs1), wal(rs3, rs4)}, false},
		"after the last":        {rs4, []string{wal(rs1, rs3)}, false},
		"spanning timelines":    {rs3, []string{wal(rs1, rt2)}, true},
		"history alone":         {rs2, []string{hist}, false},
	} {
		if _, ok := Restorable([]store.BackupRun{pbase(time.Hour, rs1, c.stop)}, c.keys, Lineage{}); ok != c.ok {
			t.Errorf("%s: usable = %v, want %v", name, ok, c.ok)
		}
	}
}

const hist2 = "1\t0/3000000\tno recovery target specified\n" // timeline 1 ended in segment 3

func TestParseLineage(t *testing.T) {
	t.Parallel()
	l := ParseLineage("00000003", "1\t0/3000060\tno recovery target\n\n2\t1/2000000\tbefore 2026-11-07\nnot a line\n3\tbad/lsn\tx\n")
	if l.Tip != "00000003" || len(l.Forks) != 2 || l.Forks["00000001"] != 3 || l.Forks["00000002"] != 256+2 {
		t.Errorf("lineage = %+v", l)
	}
}

// A base on an older timeline past the point it forked cannot reach the tip
// (postgres: "requested timeline is not a child of this server's history").
func TestPickBaseSkipsBasesPastTheFork(t *testing.T) {
	t.Parallel()
	h := time.Hour
	rt2s := "000000020000000000000004"
	keys := []string{wal(rs1, rs4), wal("000000020000000000000003", rt2s), hist}
	before := pbase(30*h, rs1, rs2) // timeline 1, before segment 3
	after := pbase(10*h, rs3, rs4)  // timeline 1, past the fork
	post := pbase(h, rt2s, rt2s)    // on the tip
	lin := ParseLineage("00000002", hist2)
	for name, c := range map[string]struct {
		bases  []store.BackupRun
		lin    Lineage
		latest bool
		at     time.Time
		want   string
	}{
		"latest skips the base past the fork":      {[]store.BackupRun{before, after}, lin, true, time.Time{}, before.ObjectKey},
		"the tip's own base wins":                  {[]store.BackupRun{before, after, post}, lin, true, time.Time{}, post.ObjectKey},
		"a time between picks the one before fork": {[]store.BackupRun{before, after}, lin, false, t0.Add(-5 * h), before.ObjectKey},
		"no history, no constraint":                {[]store.BackupRun{before, after}, Lineage{}, true, time.Time{}, after.ObjectKey},
		"a timeline outside the lineage":           {[]store.BackupRun{after}, ParseLineage("00000003", "2\t0/3000000\tx\n"), true, time.Time{}, ""},
	} {
		p, err := PickBase(c.bases, keys, c.lin, c.at, c.latest)
		switch {
		case c.want == "" && err == nil:
			t.Errorf("%s: picked %s", name, p.Base.ObjectKey)
		case c.want != "" && (err != nil || p.Base.ObjectKey != c.want):
			t.Errorf("%s: picked %s (%v), want %s", name, p.Base.ObjectKey, err, c.want)
		}
	}
	if from, ok := Restorable([]store.BackupRun{before, after}, keys, lin); !ok || !from.Equal(*before.FinishedAt) {
		t.Errorf("range starts %v (%v), want the base before the fork", from, ok)
	}
	if _, ok := Restorable([]store.BackupRun{after}, keys, lin); ok {
		t.Error("the range counts a base past the fork")
	}
}

// A restore refuses a plan whose WAL has a hole, naming the first missing
// segment; a timeline switch along the lineage is not a hole.
func TestPickBaseRefusesAWALHole(t *testing.T) {
	t.Parallel()
	b := pbase(time.Hour, rs1, rs2)
	_, err := PickBase([]store.BackupRun{b}, []string{wal(rs1, rs2), wal(rs4, rs4)}, Lineage{}, time.Time{}, true)
	if err == nil || !strings.Contains(err.Error(), rs3) {
		t.Errorf("hole at %s: err = %v", rs3, err)
	}
	if _, err := PickBase([]store.BackupRun{b}, []string{wal(rs1, rs4)}, Lineage{}, time.Time{}, true); err != nil {
		t.Errorf("contiguous WAL refused: %v", err)
	}
	lin := ParseLineage("00000002", hist2)
	keys := []string{wal(rs1, rs2), wal("000000020000000000000003", "000000020000000000000005"), hist}
	if _, err := PickBase([]store.BackupRun{b}, keys, lin, time.Time{}, true); err != nil {
		t.Errorf("a timeline switch refused: %v", err)
	}
	keys = []string{wal(rs1, rs1), wal("000000020000000000000003", "000000020000000000000005"), hist}
	b = pbase(time.Hour, rs1, rs1)
	if _, err := PickBase([]store.BackupRun{b}, keys, lin, time.Time{}, true); err == nil || !strings.Contains(err.Error(), rs2) {
		t.Errorf("hole before the fork: err = %v", err)
	}
}
